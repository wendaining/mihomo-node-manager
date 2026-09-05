package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/local/mihomo-node-manager/internal/config"
	"github.com/local/mihomo-node-manager/internal/mihomo"
	"github.com/local/mihomo-node-manager/internal/state"
)

type Controller interface {
	Group(context.Context, string) (mihomo.Group, error)
	Providers(context.Context) (map[string]string, error)
	Probe(context.Context, string, string, string, string, time.Duration) (int, error)
	Select(context.Context, string, string) (mihomo.Group, error)
}

type StateStore interface {
	Load() (state.Persisted, error)
	Save(state.Persisted) error
}

type OperationError struct {
	Code string
	Err  error
}

func (e *OperationError) Error() string { return e.Err.Error() }
func (e *OperationError) Unwrap() error { return e.Err }

type Status struct {
	Status              string     `json:"status"`
	Mode                string     `json:"mode"`
	ManualUntil         *time.Time `json:"manual_until"`
	Group               string     `json:"group"`
	GroupType           string     `json:"group_type,omitempty"`
	ActualNode          string     `json:"actual_node,omitempty"`
	DesiredNode         string     `json:"desired_node,omitempty"`
	SelectedSince       *time.Time `json:"selected_since"`
	MihomoReachable     bool       `json:"mihomo_reachable"`
	PresentAllowedNodes int        `json:"present_allowed_nodes"`
	LastCycleAt         *time.Time `json:"last_cycle_at"`
	LastCycleError      string     `json:"last_cycle_error,omitempty"`
	LastDecision        string     `json:"last_decision,omitempty"`
	DryRun              bool       `json:"dry_run"`
}

type NodeStatus struct {
	Name                 string     `json:"name"`
	Present              bool       `json:"present"`
	Available            bool       `json:"available"`
	LastProbeSuccess     bool       `json:"last_probe_success"`
	LastDelayMS          int        `json:"last_delay_ms,omitempty"`
	EWMADelayMS          float64    `json:"ewma_delay_ms,omitempty"`
	SuccessRate          float64    `json:"success_rate"`
	ConsecutiveSuccesses int        `json:"consecutive_successes"`
	ConsecutiveFailures  int        `json:"consecutive_failures"`
	LastCheckedAt        *time.Time `json:"last_checked_at"`
	LastError            string     `json:"last_error,omitempty"`
}

type Snapshot struct {
	Status Status       `json:"status"`
	Nodes  []NodeStatus `json:"nodes"`
}

type probeResult struct {
	node    string
	delayMS int
	err     error
}

type Manager struct {
	cfg        config.Config
	controller Controller
	store      StateStore
	logger     *slog.Logger
	dryRun     bool
	now        func() time.Time

	cycleMu sync.Mutex
	mu      sync.RWMutex
	persist state.Persisted
	status  Status
	trigger chan struct{}
}

func New(cfg config.Config, controller Controller, store StateStore, logger *slog.Logger, dryRun bool) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	persisted, err := store.Load()
	if err != nil {
		logger.Warn("state_load_failed_using_clean_state", "error", err)
		persisted = state.New()
	}
	allowed := make(map[string]struct{}, len(cfg.AllowedNodes))
	for _, name := range cfg.AllowedNodes {
		allowed[name] = struct{}{}
	}
	for name := range persisted.Nodes {
		if _, ok := allowed[name]; !ok {
			delete(persisted.Nodes, name)
		}
	}
	for _, name := range cfg.AllowedNodes {
		if persisted.Nodes[name] == nil {
			persisted.Nodes[name] = &state.Node{Name: name}
		} else {
			persisted.Nodes[name].Name = name
		}
	}
	if _, ok := allowed[persisted.DesiredNode]; persisted.DesiredNode != "" && !ok {
		persisted.DesiredNode = ""
		persisted.SelectedSince = time.Time{}
		persisted.ManualNode = ""
		persisted.ManualUntil = time.Time{}
	}
	if _, ok := allowed[persisted.ManualNode]; persisted.ManualNode != "" && !ok {
		persisted.ManualNode = ""
		persisted.ManualUntil = time.Time{}
	}

	m := &Manager{
		cfg:        cfg,
		controller: controller,
		store:      store,
		logger:     logger,
		dryRun:     dryRun,
		now:        time.Now,
		persist:    persisted,
		trigger:    make(chan struct{}, 1),
	}
	m.status = Status{Group: cfg.Mihomo.Group, DryRun: dryRun}
	m.refreshStatusLocked(m.now())
	return m
}

func (m *Manager) Run(ctx context.Context) error {
	if err := m.RunCycle(ctx); err != nil {
		m.logger.Warn("initial_cycle_failed", "error", err)
	}
	ticker := time.NewTicker(time.Duration(m.cfg.Probe.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-m.trigger:
		}
		if err := m.RunCycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
			m.logger.Warn("cycle_failed", "error", err)
		}
	}
}

func (m *Manager) Trigger() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

func (m *Manager) RunCycle(ctx context.Context) error {
	m.cycleMu.Lock()
	defer m.cycleMu.Unlock()
	return m.runCycleLocked(ctx)
}

func (m *Manager) runCycleLocked(ctx context.Context) error {
	now := m.now()
	group, err := m.controller.Group(ctx, m.cfg.Mihomo.Group)
	if err != nil {
		m.mu.Lock()
		m.status.MihomoReachable = false
		m.status.LastCycleAt = timePtr(now)
		m.status.LastCycleError = err.Error()
		m.status.LastDecision = "Mihomo API unavailable; retained last desired node"
		m.refreshStatusLocked(now)
		m.saveLocked()
		m.mu.Unlock()
		return fmt.Errorf("get policy group: %w", err)
	}

	present := make(map[string]bool, len(group.All))
	for _, name := range group.All {
		present[name] = true
	}
	providers, providersErr := m.controller.Providers(ctx)
	if providersErr != nil {
		m.mu.Lock()
		m.status.MihomoReachable = true
		m.status.GroupType = group.Type
		m.status.ActualNode = group.Now
		m.status.LastCycleAt = timePtr(now)
		m.status.LastCycleError = providersErr.Error()
		m.status.LastDecision = "Mihomo provider inventory unavailable; retained last desired node"
		m.refreshStatusLocked(now)
		m.saveLocked()
		m.mu.Unlock()
		return fmt.Errorf("get proxy providers: %w", providersErr)
	}
	results := m.probeAll(ctx, present, providers)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.MihomoReachable = true
	m.status.GroupType = group.Type
	m.status.ActualNode = group.Now
	m.status.LastCycleAt = timePtr(now)
	m.status.LastCycleError = ""
	m.status.PresentAllowedNodes = 0
	for _, name := range m.cfg.AllowedNodes {
		node := m.persist.Nodes[name]
		node.Present = present[name]
		if !node.Present {
			m.recordProbeLocked(node, now, 0, errors.New("node is not present in the configured Mihomo group"))
			continue
		}
		m.status.PresentAllowedNodes++
		result := results[name]
		m.recordProbeLocked(node, now, result.delayMS, result.err)
		if result.err != nil {
			m.logger.Warn("node_probe_failed", "node", name, "error", result.err, "consecutive_failures", node.ConsecutiveFailures)
		} else {
			m.logger.Debug("node_probe_succeeded", "node", name, "delay_ms", result.delayMS, "ewma_delay_ms", node.EWMADelayMS)
		}
	}

	target, reason := m.evaluateLocked(group, now)
	if target != "" {
		if m.dryRun {
			m.status.LastDecision = fmt.Sprintf("dry-run: would select %q: %s", target, reason)
			m.logger.Info("selection_dry_run", "node", target, "reason", reason)
		} else {
			actual, selectErr := m.controller.Select(ctx, m.cfg.Mihomo.Group, target)
			if selectErr != nil {
				m.status.LastCycleError = selectErr.Error()
				m.status.LastDecision = fmt.Sprintf("selection of %q failed: %s", target, reason)
				m.logger.Error("selection_failed", "node", target, "reason", reason, "error", selectErr)
				m.refreshStatusLocked(now)
				m.saveLocked()
				return fmt.Errorf("select %q: %w", target, selectErr)
			}
			changed := m.persist.DesiredNode != target
			m.persist.DesiredNode = target
			if changed || m.persist.SelectedSince.IsZero() {
				m.persist.SelectedSince = now
			}
			m.persist.BetterCandidate = ""
			m.persist.BetterRounds = 0
			m.status.ActualNode = actual.Now
			m.status.LastDecision = fmt.Sprintf("selected %q: %s", target, reason)
			m.logger.Info("node_selected", "node", target, "reason", reason)
		}
	} else if reason != "" {
		m.status.LastDecision = reason
	}
	m.refreshStatusLocked(now)
	m.saveLocked()
	return nil
}

func (m *Manager) probeAll(ctx context.Context, present map[string]bool, providers map[string]string) map[string]probeResult {
	results := make(map[string]probeResult, len(m.cfg.AllowedNodes))
	resultCh := make(chan probeResult, len(m.cfg.AllowedNodes))
	sem := make(chan struct{}, m.cfg.Probe.Concurrency)
	var wg sync.WaitGroup
	probeTimeout := time.Duration(m.cfg.Probe.TimeoutMS) * time.Millisecond
	for _, name := range m.cfg.AllowedNodes {
		if !present[name] {
			continue
		}
		provider, ok := providers[name]
		if !ok {
			results[name] = probeResult{node: name, err: errors.New("node was not found in the Mihomo provider inventory")}
			continue
		}
		wg.Add(1)
		go func(provider, node string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				resultCh <- probeResult{node: node, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			probeCtx, cancel := context.WithTimeout(ctx, probeTimeout+time.Second)
			defer cancel()
			delay, err := m.controller.Probe(probeCtx, provider, node, m.cfg.Probe.URL, m.cfg.Probe.ExpectedStatus, probeTimeout)
			resultCh <- probeResult{node: node, delayMS: delay, err: err}
		}(provider, name)
	}
	go func() {
		wg.Wait()
		close(resultCh)
	}()
	for result := range resultCh {
		results[result.node] = result
	}
	return results
}

func (m *Manager) recordProbeLocked(node *state.Node, now time.Time, delay int, probeErr error) {
	wasKnown := node.Known
	node.Known = true
	node.LastCheckedAt = now
	if probeErr == nil {
		node.LastProbeSuccess = true
		node.LastDelayMS = delay
		node.LastError = ""
		node.ConsecutiveSuccesses++
		node.ConsecutiveFailures = 0
		if node.EWMADelayMS == 0 {
			node.EWMADelayMS = float64(delay)
		} else {
			a := m.cfg.Policy.EWMAAlpha
			node.EWMADelayMS = a*float64(delay) + (1-a)*node.EWMADelayMS
		}
		if !wasKnown || node.Available || node.ConsecutiveSuccesses >= m.cfg.Policy.RecoveryThreshold {
			node.Available = true
		}
	} else {
		node.LastProbeSuccess = false
		node.LastDelayMS = 0
		node.LastError = probeErr.Error()
		node.ConsecutiveFailures++
		node.ConsecutiveSuccesses = 0
		if !wasKnown || node.ConsecutiveFailures >= m.cfg.Policy.FailureThreshold {
			node.Available = false
		}
	}
	node.Samples = append(node.Samples, state.Sample{At: now, Success: probeErr == nil, DelayMS: delay})
	if extra := len(node.Samples) - m.cfg.Probe.HistorySize; extra > 0 {
		node.Samples = append([]state.Sample(nil), node.Samples[extra:]...)
	}
}

func (m *Manager) evaluateLocked(group mihomo.Group, now time.Time) (string, string) {
	if !m.persist.ManualUntil.IsZero() && !now.Before(m.persist.ManualUntil) {
		m.persist.ManualUntil = time.Time{}
		m.persist.ManualNode = ""
		m.persist.BetterCandidate = ""
		m.persist.BetterRounds = 0
		m.status.LastDecision = "manual override expired; resumed automatic selection"
	}

	if m.persist.ManualNode != "" && now.Before(m.persist.ManualUntil) {
		manual := m.persist.Nodes[m.persist.ManualNode]
		if manual == nil || !manual.Present || manual.ConsecutiveFailures >= m.cfg.Policy.FailureThreshold {
			failed := m.persist.ManualNode
			m.persist.ManualNode = ""
			m.persist.ManualUntil = time.Time{}
			m.persist.BetterCandidate = ""
			m.persist.BetterRounds = 0
			if best := m.bestEligibleLocked(failed); best != "" {
				return best, fmt.Sprintf("manual node %q failed; emergency failover", failed)
			}
			return m.reconcileLocked(group, fmt.Sprintf("manual node %q failed but no healthy alternative is available", failed))
		}
		m.persist.DesiredNode = m.persist.ManualNode
		if selectionDrift(group, m.persist.ManualNode) {
			return m.persist.ManualNode, "reconcile active manual override"
		}
		return "", fmt.Sprintf("manual override keeps %q until %s", m.persist.ManualNode, m.persist.ManualUntil.Format(time.RFC3339))
	}

	current := m.persist.DesiredNode
	if current == "" {
		if node := m.persist.Nodes[group.Now]; node != nil && node.Present && node.LastProbeSuccess {
			m.persist.DesiredNode = group.Now
			m.persist.SelectedSince = now
			if selectionDrift(group, group.Now) {
				return group.Now, "pin currently active allowed node after startup"
			}
			return "", fmt.Sprintf("adopted current allowed node %q", group.Now)
		}
		if best := m.bestEligibleLocked(""); best != "" {
			return best, "initial selection of lowest-latency healthy allowed node"
		}
		return "", "no allowed node has a successful probe; no selection issued"
	}

	currentState := m.persist.Nodes[current]
	if currentState == nil || !currentState.Present || currentState.ConsecutiveFailures >= m.cfg.Policy.FailureThreshold {
		if best := m.bestEligibleLocked(current); best != "" {
			return best, fmt.Sprintf("current node %q is unavailable; emergency failover", current)
		}
		return m.reconcileLocked(group, fmt.Sprintf("current node %q is unavailable but no healthy alternative exists", current))
	}

	best := m.bestEligibleLocked(current)
	if best != "" && m.materiallyBetterLocked(best, current) {
		if m.persist.BetterCandidate == best {
			m.persist.BetterRounds++
		} else {
			m.persist.BetterCandidate = best
			m.persist.BetterRounds = 1
		}
		if now.Sub(m.persist.SelectedSince) >= time.Duration(m.cfg.Policy.MinDwellSeconds)*time.Second && m.persist.BetterRounds >= m.cfg.Policy.BetterRounds {
			return best, fmt.Sprintf("candidate was materially faster for %d consecutive rounds", m.persist.BetterRounds)
		}
	} else {
		m.persist.BetterCandidate = ""
		m.persist.BetterRounds = 0
	}

	return m.reconcileLocked(group, fmt.Sprintf("kept %q; no stable switch condition met", current))
}

func (m *Manager) reconcileLocked(group mihomo.Group, noActionReason string) (string, string) {
	current := m.persist.DesiredNode
	if current != "" {
		if node := m.persist.Nodes[current]; node != nil && node.Present && selectionDrift(group, current) {
			return current, "reconcile Mihomo selection drift"
		}
	}
	return "", noActionReason
}

func selectionDrift(group mihomo.Group, desired string) bool {
	if group.Now != desired {
		return true
	}
	return (group.Type == "URLTest" || group.Type == "Fallback") && group.Fixed != desired
}

func (m *Manager) materiallyBetterLocked(candidate, current string) bool {
	candidateDelay := m.persist.Nodes[candidate].EWMADelayMS
	currentDelay := m.persist.Nodes[current].EWMADelayMS
	if candidateDelay <= 0 || currentDelay <= 0 || candidateDelay >= currentDelay {
		return false
	}
	difference := currentDelay - candidateDelay
	return difference >= float64(m.cfg.Policy.ImprovementMS) || candidateDelay <= currentDelay*(1-m.cfg.Policy.ImprovementRatio)
}

func (m *Manager) bestEligibleLocked(exclude string) string {
	best := ""
	bestDelay := 0.0
	for _, name := range m.cfg.AllowedNodes {
		if name == exclude {
			continue
		}
		node := m.persist.Nodes[name]
		if node == nil || !node.Present || !node.Available || !node.LastProbeSuccess || node.EWMADelayMS <= 0 {
			continue
		}
		if best == "" || node.EWMADelayMS < bestDelay {
			best = name
			bestDelay = node.EWMADelayMS
		}
	}
	return best
}

func (m *Manager) ManualSwitch(ctx context.Context, nodeName string, force bool) (Snapshot, error) {
	m.cycleMu.Lock()
	defer m.cycleMu.Unlock()
	if !m.allowed(nodeName) {
		return m.Snapshot(), &OperationError{Code: "node_not_allowed", Err: fmt.Errorf("node %q is not in allowed_nodes", nodeName)}
	}
	if m.dryRun {
		return m.Snapshot(), &OperationError{Code: "dry_run", Err: errors.New("manual switching is disabled in dry-run mode")}
	}
	group, err := m.controller.Group(ctx, m.cfg.Mihomo.Group)
	if err != nil {
		return m.Snapshot(), &OperationError{Code: "mihomo_unavailable", Err: err}
	}
	present := false
	for _, member := range group.All {
		if member == nodeName {
			present = true
			break
		}
	}
	if !present {
		return m.Snapshot(), &OperationError{Code: "node_not_present", Err: fmt.Errorf("node %q is not present in group %q", nodeName, m.cfg.Mihomo.Group)}
	}
	now := m.now()
	if !force {
		providers, providersErr := m.controller.Providers(ctx)
		if providersErr != nil {
			return m.Snapshot(), &OperationError{Code: "mihomo_unavailable", Err: providersErr}
		}
		provider, ok := providers[nodeName]
		if !ok {
			return m.Snapshot(), &OperationError{Code: "node_not_present", Err: fmt.Errorf("node %q is missing from the Mihomo provider inventory", nodeName)}
		}
		delay, probeErr := m.controller.Probe(ctx, provider, nodeName, m.cfg.Probe.URL, m.cfg.Probe.ExpectedStatus, time.Duration(m.cfg.Probe.TimeoutMS)*time.Millisecond)
		m.mu.Lock()
		node := m.persist.Nodes[nodeName]
		node.Present = true
		m.recordProbeLocked(node, now, delay, probeErr)
		m.saveLocked()
		m.mu.Unlock()
		if probeErr != nil {
			return m.Snapshot(), &OperationError{Code: "probe_failed", Err: fmt.Errorf("probe %q: %w", nodeName, probeErr)}
		}
	}
	actual, err := m.controller.Select(ctx, m.cfg.Mihomo.Group, nodeName)
	if err != nil {
		return m.Snapshot(), &OperationError{Code: "selection_failed", Err: err}
	}
	m.mu.Lock()
	m.persist.DesiredNode = nodeName
	m.persist.SelectedSince = now
	m.persist.ManualNode = nodeName
	m.persist.ManualUntil = now.Add(time.Duration(m.cfg.Policy.ManualOverrideSeconds) * time.Second)
	m.persist.BetterCandidate = ""
	m.persist.BetterRounds = 0
	m.status.MihomoReachable = true
	m.status.GroupType = actual.Type
	m.status.ActualNode = actual.Now
	m.status.LastDecision = fmt.Sprintf("manual switch to %q", nodeName)
	m.refreshStatusLocked(now)
	m.saveLocked()
	m.mu.Unlock()
	m.logger.Info("manual_switch", "node", nodeName, "force", force, "until", now.Add(time.Duration(m.cfg.Policy.ManualOverrideSeconds)*time.Second))
	return m.Snapshot(), nil
}

func (m *Manager) ResumeAuto(ctx context.Context) (Snapshot, error) {
	m.cycleMu.Lock()
	m.mu.Lock()
	m.persist.ManualNode = ""
	m.persist.ManualUntil = time.Time{}
	m.persist.BetterCandidate = ""
	m.persist.BetterRounds = 0
	m.status.LastDecision = "manual override ended by API request"
	m.saveLocked()
	m.mu.Unlock()
	err := m.runCycleLocked(ctx)
	m.cycleMu.Unlock()
	if err != nil {
		return m.Snapshot(), &OperationError{Code: "cycle_failed", Err: err}
	}
	return m.Snapshot(), nil
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := m.now()
	status := m.status
	status.Mode = m.modeLocked(now)
	nodes := make([]NodeStatus, 0, len(m.cfg.AllowedNodes))
	for _, name := range m.cfg.AllowedNodes {
		node := m.persist.Nodes[name]
		view := NodeStatus{Name: name}
		if node != nil {
			view.Present = node.Present
			view.Available = node.Available
			view.LastProbeSuccess = node.LastProbeSuccess
			view.LastDelayMS = node.LastDelayMS
			view.EWMADelayMS = node.EWMADelayMS
			view.SuccessRate = successRate(node.Samples)
			view.ConsecutiveSuccesses = node.ConsecutiveSuccesses
			view.ConsecutiveFailures = node.ConsecutiveFailures
			view.LastError = node.LastError
			if !node.LastCheckedAt.IsZero() {
				view.LastCheckedAt = timePtr(node.LastCheckedAt)
			}
		}
		nodes = append(nodes, view)
	}
	return Snapshot{Status: status, Nodes: nodes}
}

func (m *Manager) Healthy() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status.MihomoReachable && m.status.PresentAllowedNodes > 0
}

func (m *Manager) allowed(name string) bool {
	for _, allowed := range m.cfg.AllowedNodes {
		if name == allowed {
			return true
		}
	}
	return false
}

func (m *Manager) refreshStatusLocked(now time.Time) {
	m.status.DesiredNode = m.persist.DesiredNode
	m.status.Mode = m.modeLocked(now)
	if m.status.MihomoReachable && m.status.PresentAllowedNodes > 0 {
		m.status.Status = "ok"
	} else {
		m.status.Status = "degraded"
	}
	if m.persist.SelectedSince.IsZero() {
		m.status.SelectedSince = nil
	} else {
		m.status.SelectedSince = timePtr(m.persist.SelectedSince)
	}
	if m.status.Mode == "manual" {
		m.status.ManualUntil = timePtr(m.persist.ManualUntil)
	} else {
		m.status.ManualUntil = nil
	}
}

func (m *Manager) modeLocked(now time.Time) string {
	if m.persist.ManualNode != "" && now.Before(m.persist.ManualUntil) {
		return "manual"
	}
	return "auto"
}

func (m *Manager) saveLocked() {
	if m.dryRun {
		return
	}
	if err := m.store.Save(m.persist); err != nil {
		m.logger.Error("state_save_failed", "error", err)
	}
}

func successRate(samples []state.Sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	successes := 0
	for _, sample := range samples {
		if sample.Success {
			successes++
		}
	}
	return float64(successes) / float64(len(samples))
}

func timePtr(value time.Time) *time.Time {
	copy := value
	return &copy
}

func SortNodesByDelay(nodes []NodeStatus) {
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].EWMADelayMS == 0 {
			return false
		}
		if nodes[j].EWMADelayMS == 0 {
			return true
		}
		return nodes[i].EWMADelayMS < nodes[j].EWMADelayMS
	})
}
