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
	"github.com/local/mihomo-node-manager/internal/pingpong"
	"github.com/local/mihomo-node-manager/internal/state"
)

type Controller interface {
	Group(context.Context, string) (mihomo.Group, error)
	Providers(context.Context) (map[string]string, error)
	Probe(context.Context, string, string, string, string, time.Duration) (int, error)
	Select(context.Context, string, string) (mihomo.Group, error)
}

// ConnectionCloser is an optional controller capability. Mihomo keeps routing
// established connections through their original node after a group switch, so
// dropping the group's connections is required both for fast failover and for
// honest per-node ping-pong results.
type ConnectionCloser interface {
	CloseGroupConnections(ctx context.Context, group string) (int, error)
}

// PingPongTester performs one CPA Gemini completion through the node the
// policy group currently resolves to. The manager switches the group before
// calling it when the node under test differs from the active one.
type PingPongTester interface {
	Test(ctx context.Context) pingpong.Result
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
	PingpongEnabled     bool       `json:"pingpong_enabled"`
	PresentAllowedNodes int        `json:"present_allowed_nodes"`
	LastCycleAt         *time.Time `json:"last_cycle_at"`
	LastCycleError      string     `json:"last_cycle_error,omitempty"`
	LastDecision        string     `json:"last_decision,omitempty"`
	DryRun              bool       `json:"dry_run"`
}

// NodePingpongView exposes the stored Gemini ping-pong verdict of one node.
// "unknown" covers never-tested, stale and inconclusive results alike.
type NodePingpongView struct {
	Status    string     `json:"status"`
	CheckedAt *time.Time `json:"checked_at"`
	LatencyMS int        `json:"latency_ms,omitempty"`
	Detail    string     `json:"detail,omitempty"`
}

type NodeStatus struct {
	Name                 string           `json:"name"`
	Present              bool             `json:"present"`
	Available            bool             `json:"available"`
	LastProbeSuccess     bool             `json:"last_probe_success"`
	LastDelayMS          int              `json:"last_delay_ms,omitempty"`
	EWMADelayMS          float64          `json:"ewma_delay_ms,omitempty"`
	SuccessRate          float64          `json:"success_rate"`
	ConsecutiveSuccesses int              `json:"consecutive_successes"`
	ConsecutiveFailures  int              `json:"consecutive_failures"`
	LastCheckedAt        *time.Time       `json:"last_checked_at"`
	LastError            string           `json:"last_error,omitempty"`
	Pingpong             NodePingpongView `json:"pingpong"`
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
	pingpong   PingPongTester

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
	if cfg.Pingpong.Active() {
		rules := pingpong.DefaultRules()
		dirtyMatch := "default"
		if dm := cfg.Pingpong.DirtyMatch; dm != nil {
			rules = pingpong.Rules{Status: dm.Status, BodyContains: dm.BodyContains}
			dirtyMatch = "custom"
		}
		m.pingpong = pingpong.NewWithRules(cfg.Pingpong.BaseURL, cfg.Pingpong.APIKey, cfg.Pingpong.Model, cfg.Pingpong.Prompt, cfg.Pingpong.MaxTokens, cfg.Pingpong.TimeoutSeconds, rules)
		logger.Info("pingpong_enabled", "model", cfg.Pingpong.Model, "safe_node", cfg.Pingpong.SafeNode, "refresh_interval_seconds", cfg.Pingpong.RefreshIntervalSeconds, "dirty_match", dirtyMatch)
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
	m.mu.Unlock()

	// Re-test the node traffic currently exits through when its ping-pong
	// verdict is missing or stale. This needs no group switch, so it never
	// disturbs user traffic; it is what detects the current node turning dirty.
	m.refreshPingpongCurrent(ctx, group, present, now)

	m.mu.Lock()
	target, reason := m.evaluateLocked(group, now)
	needsTest := m.needsPingpongTestLocked(target, now)
	m.mu.Unlock()

	if needsTest && m.dryRun {
		m.mu.Lock()
		m.status.LastDecision = fmt.Sprintf("dry-run: would run the Gemini ping-pong test on %q before selecting it: %s", target, reason)
		m.logger.Info("pingpong_test_dry_run", "node", target, "reason", reason)
		m.refreshStatusLocked(now)
		m.mu.Unlock()
		return nil
	}
	tested := false
	if needsTest {
		// Verify an unproven candidate with the Gemini ping-pong test before
		// committing it; dirty candidates are recorded and skipped.
		target, reason, tested = m.resolveUnknownTarget(ctx, &group, target, reason, now)
	}
	return m.commitSelection(ctx, group, target, reason, tested, now)
}

// commitSelection applies the cycle's decision: select the target through the
// Mihomo API, persist it, and record why. It expects the manager lock to be
// free and acquires it itself.
func (m *Manager) commitSelection(ctx context.Context, group mihomo.Group, target, reason string, tested bool, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if target == "" {
		if reason != "" {
			m.status.LastDecision = reason
		}
		m.refreshStatusLocked(now)
		m.saveLocked()
		return nil
	}
	if m.dryRun {
		m.status.LastDecision = fmt.Sprintf("dry-run: would select %q: %s", target, reason)
		m.logger.Info("selection_dry_run", "node", target, "reason", reason)
		m.refreshStatusLocked(now)
		return nil
	}
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
	if changed && !tested {
		m.closeGroupConnections(ctx)
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
			if best := m.bestEligibleLocked(failed, now); best != "" {
				return best, fmt.Sprintf("manual node %q failed; emergency failover", failed)
			}
			return m.reconcileLocked(group, fmt.Sprintf("manual node %q failed but no healthy alternative is available", failed))
		}
		if m.pingpongStateLocked(manual, now) == pingpong.StatusDirty {
			// The manual node still has connectivity but Google now rejects its
			// egress; keeping it would keep producing 400s.
			if best := m.bestEligibleLocked("", now); best != "" && best != m.persist.ManualNode {
				failed := m.persist.ManualNode
				m.persist.ManualNode = ""
				m.persist.ManualUntil = time.Time{}
				m.persist.BetterCandidate = ""
				m.persist.BetterRounds = 0
				return best, fmt.Sprintf("manual node %q failed the Gemini ping-pong test; emergency failover", failed)
			}
			m.status.LastDecision = fmt.Sprintf("manual node %q failed the Gemini ping-pong test but no better allowed node is known; keeping it", m.persist.ManualNode)
		}
		m.persist.DesiredNode = m.persist.ManualNode
		if selectionDrift(group, m.persist.ManualNode) {
			return m.persist.ManualNode, "reconcile active manual override"
		}
		return "", fmt.Sprintf("manual override keeps %q until %s", m.persist.ManualNode, m.persist.ManualUntil.Format(time.RFC3339))
	}

	current := m.persist.DesiredNode
	if current == "" {
		if node := m.persist.Nodes[group.Now]; node != nil && node.Present && node.LastProbeSuccess && m.pingpongStateLocked(node, now) != pingpong.StatusDirty {
			m.persist.DesiredNode = group.Now
			m.persist.SelectedSince = now
			if selectionDrift(group, group.Now) {
				return group.Now, "pin currently active allowed node after startup"
			}
			return "", fmt.Sprintf("adopted current allowed node %q", group.Now)
		}
		if best := m.bestEligibleLocked("", now); best != "" {
			return best, "initial selection of lowest-latency healthy allowed node"
		}
		return "", "no allowed node has a successful probe; no selection issued"
	}

	currentState := m.persist.Nodes[current]
	if currentState == nil || !currentState.Present || currentState.ConsecutiveFailures >= m.cfg.Policy.FailureThreshold {
		if best := m.bestEligibleLocked(current, now); best != "" {
			return best, fmt.Sprintf("current node %q is unavailable; emergency failover", current)
		}
		return m.reconcileLocked(group, fmt.Sprintf("current node %q is unavailable but no healthy alternative exists", current))
	}
	if m.pingpongStateLocked(currentState, now) == pingpong.StatusDirty {
		if safe := m.safeNodeCandidateLocked(current, now); safe != "" {
			return safe, fmt.Sprintf("current node %q failed the Gemini ping-pong test; trying the configured safe node", current)
		}
		if best := m.bestEligibleLocked("", now); best != "" && best != current {
			return best, fmt.Sprintf("current node %q failed the Gemini ping-pong test; emergency failover", current)
		}
		return m.reconcileLocked(group, fmt.Sprintf("current node %q failed the Gemini ping-pong test but no better allowed node exists; keeping it", current))
	}

	best := m.bestEligibleLocked(current, now)
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

// bestEligibleLocked returns the fastest probe-healthy allowed node under the
// Gemini ping-pong constraint, in strict priority order:
//
//  1. among nodes known to pass the ping-pong test,
//  2. among nodes not known to fail it (never tested, stale or inconclusive),
//  3. among all probe-healthy nodes - only when every candidate is marked
//     dirty, dropping the constraint so the fastest node still wins.
//
// With the ping-pong probe disabled the pool degenerates to the original
// pure-latency behaviour.
func (m *Manager) bestEligibleLocked(exclude string, now time.Time) string {
	alive := make([]string, 0, len(m.cfg.AllowedNodes))
	for _, name := range m.cfg.AllowedNodes {
		if name == exclude {
			continue
		}
		node := m.persist.Nodes[name]
		if node == nil || !node.Present || !node.Available || !node.LastProbeSuccess || node.EWMADelayMS <= 0 {
			continue
		}
		alive = append(alive, name)
	}
	if len(alive) == 0 {
		return ""
	}
	if m.pingpong != nil {
		passing := make([]string, 0, len(alive))
		notDirty := make([]string, 0, len(alive))
		for _, name := range alive {
			switch m.pingpongStateLocked(m.persist.Nodes[name], now) {
			case pingpong.StatusPass:
				passing = append(passing, name)
			case pingpong.StatusDirty:
			default:
				notDirty = append(notDirty, name)
			}
		}
		switch {
		case len(passing) > 0:
			return m.fastestOfLocked(passing)
		case len(notDirty) > 0:
			return m.fastestOfLocked(notDirty)
		}
		m.logger.Warn("pingpong_constraint_dropped", "reason", "every probe-healthy allowed node is marked dirty; falling back to pure latency selection")
	}
	return m.fastestOfLocked(alive)
}

func (m *Manager) fastestOfLocked(names []string) string {
	best := ""
	bestDelay := 0.0
	for _, name := range names {
		delay := m.persist.Nodes[name].EWMADelayMS
		if best == "" || delay < bestDelay {
			best = name
			bestDelay = delay
		}
	}
	return best
}

// pingpongStateLocked classifies a node's stored ping-pong verdict with its
// freshness window: "pass" stays valid for refresh_interval_seconds, "dirty"
// for fail_ttl_seconds, and anything else (never tested, inconclusive, stale)
// maps to "" - meaning "unknown, worth testing".
func (m *Manager) pingpongStateLocked(node *state.Node, now time.Time) pingpong.Status {
	if node == nil || node.PingpongCheckedAt.IsZero() {
		return ""
	}
	age := now.Sub(node.PingpongCheckedAt)
	switch pingpong.Status(node.PingpongStatus) {
	case pingpong.StatusPass:
		if age <= time.Duration(m.cfg.Pingpong.RefreshIntervalSeconds)*time.Second {
			return pingpong.StatusPass
		}
	case pingpong.StatusDirty:
		if age <= time.Duration(m.cfg.Pingpong.FailTTLSeconds)*time.Second {
			return pingpong.StatusDirty
		}
	}
	return ""
}

// pingpongStaleLocked reports whether a node's stored verdict should be
// refreshed by a new test. Inconclusive results also age out so a CPA outage
// does not pin stale knowledge forever.
func (m *Manager) pingpongStaleLocked(node *state.Node, now time.Time) bool {
	if node == nil || node.PingpongStatus == "" || node.PingpongCheckedAt.IsZero() {
		return true
	}
	return now.Sub(node.PingpongCheckedAt) >= time.Duration(m.cfg.Pingpong.RefreshIntervalSeconds)*time.Second
}

// needsPingpongTestLocked reports whether selecting name requires a ping-pong
// test first: the probe must be active, the node must differ from the already
// committed selection, and its verdict must be unknown.
func (m *Manager) needsPingpongTestLocked(name string, now time.Time) bool {
	if m.pingpong == nil || name == "" || name == m.persist.DesiredNode {
		return false
	}
	return m.pingpongStateLocked(m.persist.Nodes[name], now) == ""
}

func (m *Manager) recordPingpongLocked(node *state.Node, result pingpong.Result, now time.Time) {
	if node == nil {
		return
	}
	node.PingpongStatus = string(result.Status)
	node.PingpongCheckedAt = now
	node.PingpongLatencyMS = result.LatencyMS
	node.PingpongDetail = result.Detail
}

// safeNodeCandidateLocked returns the configured safe node when it is a
// plausible alternative to a dirty current node: present, healthy and not
// itself known dirty. It is a testing preference, never an unconditional
// choice - the candidate still has to pass the ping-pong test.
func (m *Manager) safeNodeCandidateLocked(current string, now time.Time) string {
	safe := m.cfg.Pingpong.SafeNode
	if safe == "" || safe == current {
		return ""
	}
	node := m.persist.Nodes[safe]
	if node == nil || !node.Present || !node.Available || !node.LastProbeSuccess {
		return ""
	}
	if m.pingpongStateLocked(node, now) == pingpong.StatusDirty {
		return ""
	}
	return safe
}

// refreshPingpongCurrent re-tests the node traffic currently exits through
// whenever its verdict is missing or stale. No group switch is involved.
func (m *Manager) refreshPingpongCurrent(ctx context.Context, group mihomo.Group, present map[string]bool, now time.Time) {
	if m.pingpong == nil {
		return
	}
	m.mu.Lock()
	current := m.persist.DesiredNode
	if current == "" {
		current = group.Now
	}
	node := m.persist.Nodes[current]
	needed := current != "" && present[current] && node != nil && node.Available && m.pingpongStaleLocked(node, now)
	m.mu.Unlock()
	if !needed {
		return
	}
	result := m.pingpong.Test(ctx)
	m.mu.Lock()
	m.recordPingpongLocked(m.persist.Nodes[current], result, now)
	m.mu.Unlock()
	m.logger.Info("pingpong_current_tested", "node", current, "status", result.Status, "latency_ms", result.LatencyMS, "detail", result.Detail)
}

// runPingpongTest points the policy group at target (unless traffic already
// exits through it), drops the group's stale connections so the CPA re-dials
// through the new node, and performs one ping-pong request.
func (m *Manager) runPingpongTest(ctx context.Context, group *mihomo.Group, target string) pingpong.Result {
	if group.Now != target {
		actual, err := m.controller.Select(ctx, m.cfg.Mihomo.Group, target)
		if err != nil {
			return pingpong.Result{Status: pingpong.StatusInconclusive, Detail: fmt.Sprintf("switching to %q for the test failed: %v", target, err)}
		}
		group.Now = actual.Now
		group.Fixed = actual.Fixed
	}
	if !m.dryRun {
		m.closeGroupConnections(ctx)
	}
	return m.pingpong.Test(ctx)
}

// resolveUnknownTarget runs the ping-pong test on candidates whose verdict is
// unknown, switching the group to each candidate for the duration of its test.
// Dirty candidates are recorded and excluded, and the next candidate is picked
// by the ordinary selection policy. The first candidate that passes - or that
// comes back inconclusive - is returned for the caller to commit.
func (m *Manager) resolveUnknownTarget(ctx context.Context, group *mihomo.Group, target, reason string, now time.Time) (string, string, bool) {
	tested := false
	for attempt := 0; target != "" && attempt <= len(m.cfg.AllowedNodes); attempt++ {
		result := m.runPingpongTest(ctx, group, target)
		tested = true
		m.mu.Lock()
		m.recordPingpongLocked(m.persist.Nodes[target], result, now)
		if result.Status != pingpong.StatusDirty {
			m.mu.Unlock()
			if result.Status == pingpong.StatusPass {
				reason += " (Gemini ping-pong passed)"
			} else {
				reason += fmt.Sprintf(" (Gemini ping-pong inconclusive: %s)", result.Detail)
			}
			return target, reason, tested
		}
		m.logger.Warn("pingpong_candidate_dirty", "node", target, "detail", result.Detail)
		target, reason = m.evaluateLocked(*group, now)
		done := target == "" || !m.needsPingpongTestLocked(target, now)
		m.mu.Unlock()
		if done {
			return target, reason, tested
		}
	}
	return target, reason, tested
}

// closeGroupConnections drops every active connection that traverses the
// policy group so clients re-dial through the newly selected node. Best
// effort by design; controlled by pingpong.close_conns_on_switch.
func (m *Manager) closeGroupConnections(ctx context.Context) {
	if !m.cfg.Pingpong.CloseConnsOnSwitch {
		return
	}
	closer, ok := m.controller.(ConnectionCloser)
	if !ok {
		return
	}
	closed, err := closer.CloseGroupConnections(ctx, m.cfg.Mihomo.Group)
	if err != nil {
		m.logger.Warn("close_group_connections_failed", "group", m.cfg.Mihomo.Group, "closed", closed, "error", err)
		return
	}
	if closed > 0 {
		m.logger.Info("closed_group_connections", "group", m.cfg.Mihomo.Group, "closed", closed)
	}
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
	if gateErr := m.pingpongGate(ctx, &group, nodeName, force, now); gateErr != nil {
		return m.Snapshot(), gateErr
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

// pingpongGate refuses manual switches to nodes that are known or suspected to
// fail the Gemini ping-pong test. A fresh pass is accepted silently; an unknown
// or stale verdict is tested first, switching the group to the candidate for
// the duration of the request and restoring the previous selection when the
// candidate comes back dirty. force=true bypasses the gate entirely.
func (m *Manager) pingpongGate(ctx context.Context, group *mihomo.Group, nodeName string, force bool, now time.Time) error {
	if m.pingpong == nil || force {
		return nil
	}
	m.mu.Lock()
	node := m.persist.Nodes[nodeName]
	verdict := m.pingpongStateLocked(node, now)
	stale := m.pingpongStaleLocked(node, now)
	m.mu.Unlock()
	if verdict == pingpong.StatusDirty {
		return &OperationError{Code: "pingpong_failed", Err: fmt.Errorf("node %q recently failed the Gemini ping-pong test; use force to override", nodeName)}
	}
	if verdict == pingpong.StatusPass && !stale {
		return nil
	}
	previous := group.Now
	result := m.runPingpongTest(ctx, group, nodeName)
	m.mu.Lock()
	m.recordPingpongLocked(m.persist.Nodes[nodeName], result, now)
	m.mu.Unlock()
	switch result.Status {
	case pingpong.StatusPass:
		return nil
	case pingpong.StatusInconclusive:
		m.logger.Warn("pingpong_gate_inconclusive", "node", nodeName, "detail", result.Detail)
		return nil
	default:
		if previous != nodeName {
			if _, err := m.controller.Select(ctx, m.cfg.Mihomo.Group, previous); err != nil {
				m.logger.Error("pingpong_gate_restore_failed", "node", previous, "error", err)
			} else {
				group.Now = previous
				group.Fixed = previous
			}
		}
		return &OperationError{Code: "pingpong_failed", Err: fmt.Errorf("node %q failed the Gemini ping-pong test (%s); selection restored to %q; use force to override", nodeName, result.Detail, previous)}
	}
}

// PingpongCheckResult is one node's outcome of an on-demand ping-pong check.
type PingpongCheckResult struct {
	Node      string `json:"node"`
	Status    string `json:"status"`
	LatencyMS int    `json:"latency_ms,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Error     string `json:"error,omitempty"`
}

// PingpongReport is the response of POST /v1/pingpong.
type PingpongReport struct {
	Results  []PingpongCheckResult `json:"results"`
	Snapshot Snapshot              `json:"snapshot"`
}

// PingpongCheck runs the CPA Gemini ping-pong test on demand. Without a node
// name it tests whatever the traffic currently exits through (no switching).
// With a node name it briefly switches to that node, tests it, and restores
// the previous selection. With force it sweeps every present allowed node and
// lands the group on the node the ordinary selection policy would pick.
func (m *Manager) PingpongCheck(ctx context.Context, nodeName string, force bool) (PingpongReport, error) {
	m.cycleMu.Lock()
	defer m.cycleMu.Unlock()
	report := PingpongReport{Results: []PingpongCheckResult{}}
	if m.pingpong == nil {
		return report, &OperationError{Code: "pingpong_disabled", Err: errors.New("the Gemini ping-pong test is not configured; set CPA_BASE_URL and CPA_MODEL")}
	}
	group, err := m.controller.Group(ctx, m.cfg.Mihomo.Group)
	if err != nil {
		return report, &OperationError{Code: "mihomo_unavailable", Err: err}
	}
	present := make(map[string]bool, len(group.All))
	for _, member := range group.All {
		present[member] = true
	}
	now := m.now()
	if force {
		if m.dryRun {
			return report, &OperationError{Code: "dry_run", Err: errors.New("a full ping-pong sweep switches nodes and is disabled in dry-run mode")}
		}
		m.runPingpongSweep(ctx, &group, present, now, &report)
		report.Snapshot = m.Snapshot()
		return report, nil
	}
	target := nodeName
	if target == "" {
		target = group.Now
	}
	if !m.allowed(target) {
		return report, &OperationError{Code: "node_not_allowed", Err: fmt.Errorf("node %q is not in allowed_nodes", target)}
	}
	if !present[target] {
		return report, &OperationError{Code: "node_not_present", Err: fmt.Errorf("node %q is not present in group %q", target, m.cfg.Mihomo.Group)}
	}
	if m.dryRun && group.Now != target {
		return report, &OperationError{Code: "dry_run", Err: fmt.Errorf("testing %q requires switching the group, which is disabled in dry-run mode", target)}
	}
	origin := group.Now
	result := m.runPingpongTest(ctx, &group, target)
	m.mu.Lock()
	m.recordPingpongLocked(m.persist.Nodes[target], result, now)
	m.mu.Unlock()
	report.Results = append(report.Results, PingpongCheckResult{Node: target, Status: string(result.Status), LatencyMS: result.LatencyMS, Detail: result.Detail})
	if origin != target {
		if _, err := m.controller.Select(ctx, m.cfg.Mihomo.Group, origin); err != nil {
			m.logger.Error("pingpong_check_restore_failed", "node", origin, "error", err)
		} else {
			m.closeGroupConnections(ctx)
		}
	}
	report.Snapshot = m.Snapshot()
	return report, nil
}

// runPingpongSweep tests every present allowed node in turn and lands the
// group on the node the selection policy would choose with fresh verdicts for
// all of them. Manual mode is respected: the manual node is restored.
func (m *Manager) runPingpongSweep(ctx context.Context, group *mihomo.Group, present map[string]bool, now time.Time, report *PingpongReport) {
	origin := group.Now
	m.mu.Lock()
	manual := m.persist.ManualNode != "" && now.Before(m.persist.ManualUntil)
	m.mu.Unlock()
	for _, name := range m.cfg.AllowedNodes {
		if !present[name] {
			continue
		}
		result := m.runPingpongTest(ctx, group, name)
		m.mu.Lock()
		m.recordPingpongLocked(m.persist.Nodes[name], result, now)
		m.mu.Unlock()
		report.Results = append(report.Results, PingpongCheckResult{Node: name, Status: string(result.Status), LatencyMS: result.LatencyMS, Detail: result.Detail})
	}
	final := ""
	m.mu.Lock()
	if manual {
		final = m.persist.ManualNode
	} else {
		final = m.bestEligibleLocked("", now)
	}
	m.mu.Unlock()
	if final == "" {
		final = origin
	}
	if final != group.Now {
		actual, err := m.controller.Select(ctx, m.cfg.Mihomo.Group, final)
		if err != nil {
			m.logger.Error("pingpong_sweep_final_selection_failed", "node", final, "error", err)
		} else {
			group.Now = actual.Now
			group.Fixed = actual.Fixed
			m.closeGroupConnections(ctx)
		}
	}
	m.mu.Lock()
	if !manual {
		if m.persist.DesiredNode != final {
			m.persist.DesiredNode = final
			m.persist.SelectedSince = now
		}
		m.persist.BetterCandidate = ""
		m.persist.BetterRounds = 0
	}
	m.status.LastDecision = fmt.Sprintf("Gemini ping-pong sweep completed; group now on %q", final)
	m.refreshStatusLocked(now)
	m.saveLocked()
	m.mu.Unlock()
	m.logger.Info("pingpong_sweep_completed", "final_node", final, "results", len(report.Results))
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
		view := NodeStatus{Name: name, Pingpong: NodePingpongView{Status: "unknown"}}
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
			if node.PingpongStatus != "" {
				view.Pingpong.Status = node.PingpongStatus
				view.Pingpong.LatencyMS = node.PingpongLatencyMS
				view.Pingpong.Detail = node.PingpongDetail
				if !node.PingpongCheckedAt.IsZero() {
					view.Pingpong.CheckedAt = timePtr(node.PingpongCheckedAt)
				}
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
	m.status.PingpongEnabled = m.pingpong != nil
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
