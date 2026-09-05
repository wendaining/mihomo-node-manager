package manager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/local/mihomo-node-manager/internal/config"
	"github.com/local/mihomo-node-manager/internal/mihomo"
	"github.com/local/mihomo-node-manager/internal/pingpong"
	"github.com/local/mihomo-node-manager/internal/state"
)

type fakeProbe struct {
	delay int
	err   error
}

type fakeController struct {
	mu          sync.Mutex
	group       mihomo.Group
	probes      map[string]fakeProbe
	probeCounts map[string]int
	selections  []string
	groupErr    error
	selectErr   error
	closedConns int
}

func (f *fakeController) Group(context.Context, string) (mihomo.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.groupErr != nil {
		return mihomo.Group{}, f.groupErr
	}
	group := f.group
	group.All = append([]string(nil), f.group.All...)
	return group, nil
}

func (f *fakeController) Providers(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	providers := make(map[string]string)
	for _, node := range f.group.All {
		providers[node] = "subscription"
	}
	return providers, nil
}

func (f *fakeController) Probe(_ context.Context, _, node, _, _ string, _ time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeCounts[node]++
	result, ok := f.probes[node]
	if !ok {
		return 0, errors.New("no fake probe configured")
	}
	return result.delay, result.err
}

func (f *fakeController) Select(_ context.Context, _, node string) (mihomo.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.selectErr != nil {
		return mihomo.Group{}, f.selectErr
	}
	f.selections = append(f.selections, node)
	f.group.Now = node
	f.group.Fixed = node
	group := f.group
	group.All = append([]string(nil), f.group.All...)
	return group, nil
}

func (f *fakeController) setProbe(node string, delay int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes[node] = fakeProbe{delay: delay, err: err}
}

func (f *fakeController) currentNode() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.group.Now
}

func (f *fakeController) CloseGroupConnections(_ context.Context, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedConns++
	return 1, nil
}

func (f *fakeController) selected() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.selections...)
}

// fakeTester answers the ping-pong test with a per-node script. The node under
// test is whatever the fake controller currently has selected.
type fakeTester struct {
	mu         sync.Mutex
	controller *fakeController
	results    map[string]pingpong.Result
	calls      []string
}

func (f *fakeTester) Test(context.Context) pingpong.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	node := f.controller.currentNode()
	f.calls = append(f.calls, node)
	result, ok := f.results[node]
	if !ok {
		return pingpong.Result{Status: pingpong.StatusInconclusive, Detail: "no fake result"}
	}
	return result
}

func (f *fakeTester) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type memoryStore struct {
	mu    sync.Mutex
	value state.Persisted
}

func newMemoryStore() *memoryStore { return &memoryStore{value: state.New()} }

func (s *memoryStore) Load() (state.Persisted, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.value), nil
}

func (s *memoryStore) Save(value state.Persisted) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = cloneState(value)
	return nil
}

func cloneState(value state.Persisted) state.Persisted {
	payload, _ := json.Marshal(value)
	var cloned state.Persisted
	_ = json.Unmarshal(payload, &cloned)
	if cloned.Nodes == nil {
		cloned.Nodes = make(map[string]*state.Node)
	}
	return cloned
}

func testManager(t *testing.T) (*Manager, *fakeController, *memoryStore, *time.Time) {
	t.Helper()
	cfg := config.Default()
	cfg.AllowedNodes = []string{"A", "B"}
	cfg.Probe.Concurrency = 2
	cfg.Policy.MinDwellSeconds = 600
	cfg.Policy.BetterRounds = 3
	cfg.Policy.FailureThreshold = 2
	cfg.Policy.RecoveryThreshold = 2
	controller := &fakeController{
		group:       mihomo.Group{Name: "PROXY", Type: "URLTest", Now: "A", Fixed: "A", All: []string{"A", "B", "EVIL"}},
		probes:      map[string]fakeProbe{"A": {delay: 500}, "B": {delay: 200}},
		probeCounts: make(map[string]int),
	}
	store := newMemoryStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := New(cfg, controller, store, logger, false)
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	return m, controller, store, &now
}

func TestFailureThresholdTriggersEmergencyFailover(t *testing.T) {
	m, controller, _, _ := testManager(t)
	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	controller.setProbe("A", 0, errors.New("timeout"))
	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := controller.selected(); len(got) != 0 {
		t.Fatalf("selected after one failure = %v", got)
	}
	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := controller.selected(); len(got) != 1 || got[0] != "B" {
		t.Fatalf("selections = %v, want [B]", got)
	}
	if got := m.Snapshot().Status.DesiredNode; got != "B" {
		t.Fatalf("desired node = %q, want B", got)
	}
}

func TestRecoveryRequiresTwoSuccesses(t *testing.T) {
	m, controller, _, _ := testManager(t)
	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	controller.setProbe("B", 0, errors.New("down"))
	_ = m.RunCycle(context.Background())
	_ = m.RunCycle(context.Background())
	if node := findNode(t, m.Snapshot(), "B"); node.Available {
		t.Fatal("B is available after two failures")
	}
	controller.setProbe("B", 180, nil)
	_ = m.RunCycle(context.Background())
	if node := findNode(t, m.Snapshot(), "B"); node.Available {
		t.Fatal("B recovered after only one success")
	}
	_ = m.RunCycle(context.Background())
	if node := findNode(t, m.Snapshot(), "B"); !node.Available {
		t.Fatal("B did not recover after two successes")
	}
}

func TestPerformanceSwitchRequiresDwellAndThreeRounds(t *testing.T) {
	m, controller, _, now := testManager(t)
	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(601 * time.Second)
	for i := 0; i < 2; i++ {
		if err := m.RunCycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := controller.selected(); len(got) != 0 {
		t.Fatalf("selected before third better round = %v", got)
	}
	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := controller.selected(); len(got) != 1 || got[0] != "B" {
		t.Fatalf("selections = %v, want [B]", got)
	}
}

func TestManualOverridePersistsExpiresAndCanFailOver(t *testing.T) {
	t.Run("persists and expires", func(t *testing.T) {
		m, controller, store, now := testManager(t)
		_ = m.RunCycle(context.Background())
		if _, err := m.ManualSwitch(context.Background(), "B", false); err != nil {
			t.Fatal(err)
		}
		if got := m.Snapshot().Status.Mode; got != "manual" {
			t.Fatalf("mode = %q", got)
		}

		cfg := m.cfg
		restarted := New(cfg, controller, store, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
		restarted.now = func() time.Time { return *now }
		if err := restarted.RunCycle(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := restarted.Snapshot().Status.Mode; got != "manual" {
			t.Fatalf("restarted mode = %q", got)
		}
		*now = now.Add(31 * time.Minute)
		if err := restarted.RunCycle(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := restarted.Snapshot().Status.Mode; got != "auto" {
			t.Fatalf("expired mode = %q", got)
		}
	})

	t.Run("failed manual node", func(t *testing.T) {
		m, controller, _, _ := testManager(t)
		_ = m.RunCycle(context.Background())
		if _, err := m.ManualSwitch(context.Background(), "B", false); err != nil {
			t.Fatal(err)
		}
		controller.setProbe("B", 0, errors.New("down"))
		controller.setProbe("A", 100, nil)
		_ = m.RunCycle(context.Background())
		_ = m.RunCycle(context.Background())
		selections := controller.selected()
		if len(selections) < 2 || selections[len(selections)-1] != "A" {
			t.Fatalf("selections = %v, want final A", selections)
		}
		if got := m.Snapshot().Status.Mode; got != "auto" {
			t.Fatalf("mode = %q after emergency failover", got)
		}
	})
}

func TestAllFailuresRetainAllowedNodeAndNeverProbeOutsideAllowlist(t *testing.T) {
	m, controller, _, _ := testManager(t)
	_ = m.RunCycle(context.Background())
	controller.setProbe("A", 0, errors.New("down"))
	controller.setProbe("B", 0, errors.New("down"))
	_ = m.RunCycle(context.Background())
	_ = m.RunCycle(context.Background())
	if got := m.Snapshot().Status.DesiredNode; got != "A" {
		t.Fatalf("desired node = %q, want retained A", got)
	}
	if got := controller.selected(); len(got) != 0 {
		t.Fatalf("unexpected selections = %v", got)
	}
	controller.mu.Lock()
	evilProbes := controller.probeCounts["EVIL"]
	controller.mu.Unlock()
	if evilProbes != 0 {
		t.Fatalf("EVIL probe count = %d", evilProbes)
	}
	if _, err := m.ManualSwitch(context.Background(), "EVIL", true); err == nil {
		t.Fatal("manual switch to non-allowed node succeeded")
	}
}

func TestMihomoInterruptionAndSelectionFailureAreSafe(t *testing.T) {
	t.Run("group unavailable", func(t *testing.T) {
		m, controller, _, _ := testManager(t)
		controller.groupErr = errors.New("connection refused")
		if err := m.RunCycle(context.Background()); err == nil {
			t.Fatal("RunCycle() unexpectedly succeeded")
		}
		snapshot := m.Snapshot()
		if snapshot.Status.Status != "degraded" || snapshot.Status.MihomoReachable {
			t.Fatalf("status = %+v", snapshot.Status)
		}
		if got := controller.selected(); len(got) != 0 {
			t.Fatalf("unexpected selections = %v", got)
		}
	})

	t.Run("selection API failure", func(t *testing.T) {
		m, controller, _, _ := testManager(t)
		controller.group.Now = "EVIL"
		controller.group.Fixed = ""
		controller.selectErr = errors.New("PUT failed")
		if err := m.RunCycle(context.Background()); err == nil {
			t.Fatal("RunCycle() unexpectedly succeeded")
		}
		if got := m.Snapshot().Status.DesiredNode; got != "" {
			t.Fatalf("desired node committed after failed PUT: %q", got)
		}
	})
}

func findNode(t *testing.T, snapshot Snapshot, name string) NodeStatus {
	t.Helper()
	for _, node := range snapshot.Nodes {
		if node.Name == name {
			return node
		}
	}
	t.Fatalf("node %q not found", name)
	return NodeStatus{}
}

// enablePingpong attaches a scripted ping-pong tester to the manager.
func enablePingpong(m *Manager, controller *fakeController, results map[string]pingpong.Result) *fakeTester {
	tester := &fakeTester{controller: controller, results: results}
	m.pingpong = tester
	return tester
}

func pingpongStatus(t *testing.T, snapshot Snapshot, name string) string {
	t.Helper()
	return findNode(t, snapshot, name).Pingpong.Status
}

func TestPingpongDirtyCurrentFailsOverToPassingNode(t *testing.T) {
	m, controller, _, _ := testManager(t)
	tester := enablePingpong(m, controller, map[string]pingpong.Result{
		"A": {Status: pingpong.StatusDirty, Detail: "HTTP 400: User location is not supported"},
		"B": {Status: pingpong.StatusPass, Detail: "pong: pong"},
	})
	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot().Status.DesiredNode; got != "B" {
		t.Fatalf("desired node = %q, want B", got)
	}
	if got := pingpongStatus(t, m.Snapshot(), "A"); got != "dirty" {
		t.Fatalf("A pingpong = %q, want dirty", got)
	}
	if got := pingpongStatus(t, m.Snapshot(), "B"); got != "pass" {
		t.Fatalf("B pingpong = %q, want pass", got)
	}
	if got := tester.callLog(); len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("pingpong calls = %v, want [A B]", got)
	}
	controller.mu.Lock()
	closed := controller.closedConns
	controller.mu.Unlock()
	if closed == 0 {
		t.Fatal("group connections were not closed around the test switch")
	}
}

func TestPingpongPassingNodesPickFastestWithoutRetesting(t *testing.T) {
	m, controller, _, now := testManager(t)
	tester := enablePingpong(m, controller, map[string]pingpong.Result{
		"A": {Status: pingpong.StatusPass},
		"B": {Status: pingpong.StatusPass},
	})
	// Cycle 1 adopts A and tests it (pass). B is faster and known to pass,
	// so once the dwell/rounds guards are satisfied the optimization path
	// must switch to it - testing B exactly once, never re-testing A.
	_ = m.RunCycle(context.Background())
	*now = now.Add(601 * time.Second)
	for i := 0; i < 3; i++ {
		if err := m.RunCycle(context.Background()); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(60 * time.Second)
	}
	if got := m.Snapshot().Status.DesiredNode; got != "B" {
		t.Fatalf("desired node = %q, want B", got)
	}
	calls := tester.callLog()
	bCalls := 0
	for _, call := range calls {
		if call == "B" {
			bCalls++
		}
	}
	if bCalls != 1 {
		t.Fatalf("B was ping-pong tested %d times, want exactly once (calls %v)", bCalls, calls)
	}
}

func TestPingpongAllDirtyFallsBackToFastest(t *testing.T) {
	m, controller, _, _ := testManager(t)
	enablePingpong(m, controller, map[string]pingpong.Result{
		"A": {Status: pingpong.StatusDirty, Detail: "location ban"},
		"B": {Status: pingpong.StatusDirty, Detail: "location ban"},
	})
	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A is adopted but dirty; B is the fastest node and everything is dirty,
	// so the constraint is dropped and B is chosen.
	if got := m.Snapshot().Status.DesiredNode; got != "B" {
		t.Fatalf("desired node = %q, want B", got)
	}
	if got := pingpongStatus(t, m.Snapshot(), "B"); got != "dirty" {
		t.Fatalf("B pingpong = %q, want dirty", got)
	}
}

func TestPingpongSafeNodeIsTriedFirst(t *testing.T) {
	// Three nodes: A (current, turns dirty), B (the safe node, mid latency),
	// C (fastest but unproven). When A turns dirty the manager must try the
	// safe node B before the faster but unknown C.
	cfg := config.Default()
	cfg.AllowedNodes = []string{"A", "B", "C"}
	controller := &fakeController{
		group:       mihomo.Group{Name: "PROXY", Type: "URLTest", Now: "A", Fixed: "A", All: []string{"A", "B", "C"}},
		probes:      map[string]fakeProbe{"A": {delay: 500}, "B": {delay: 300}, "C": {delay: 200}},
		probeCounts: make(map[string]int),
	}
	m := New(cfg, controller, newMemoryStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	m.cfg.Pingpong.SafeNode = "B"
	results := map[string]pingpong.Result{
		"A": {Status: pingpong.StatusPass},
		"B": {Status: pingpong.StatusPass},
		"C": {Status: pingpong.StatusPass},
	}
	tester := enablePingpong(m, controller, results)

	_ = m.RunCycle(context.Background())
	if got := m.Snapshot().Status.DesiredNode; got != "A" {
		t.Fatalf("desired node after cycle 1 = %q, want A", got)
	}
	results["A"] = pingpong.Result{Status: pingpong.StatusDirty, Detail: "location ban"}
	now = now.Add(301 * time.Second)
	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot().Status.DesiredNode; got != "B" {
		t.Fatalf("desired node = %q, want the safe node B", got)
	}
	if got := tester.callLog(); len(got) != 3 || got[2] != "B" {
		t.Fatalf("pingpong calls = %v, want [... B] with B tried before C", got)
	}
	if got := pingpongStatus(t, m.Snapshot(), "C"); got != "unknown" {
		t.Fatalf("C pingpong = %q, want untouched unknown", got)
	}
}

func TestPingpongRefreshRespectsInterval(t *testing.T) {
	m, controller, _, now := testManager(t)
	tester := enablePingpong(m, controller, map[string]pingpong.Result{
		"A": {Status: pingpong.StatusPass},
		"B": {Status: pingpong.StatusPass},
	})
	_ = m.RunCycle(context.Background())
	_ = m.RunCycle(context.Background())
	if got := tester.callLog(); len(got) != 1 {
		t.Fatalf("pingpong calls = %v, want a single refresh", got)
	}
	*now = now.Add(301 * time.Second)
	_ = m.RunCycle(context.Background())
	if got := tester.callLog(); len(got) != 2 {
		t.Fatalf("pingpong calls = %v, want a refresh after the interval", got)
	}
}

func TestManualSwitchToDirtyNodeIsRejectedAndRestored(t *testing.T) {
	m, controller, _, _ := testManager(t)
	enablePingpong(m, controller, map[string]pingpong.Result{
		"A": {Status: pingpong.StatusPass},
		"B": {Status: pingpong.StatusDirty, Detail: "location ban"},
	})
	_ = m.RunCycle(context.Background())
	_, err := m.ManualSwitch(context.Background(), "B", false)
	var operationErr *OperationError
	if !errors.As(err, &operationErr) || operationErr.Code != "pingpong_failed" {
		t.Fatalf("ManualSwitch() error = %v, want pingpong_failed", err)
	}
	status := m.Snapshot().Status
	if status.Mode != "auto" || status.DesiredNode != "A" {
		t.Fatalf("status after rejection = mode %q desired %q", status.Mode, status.DesiredNode)
	}
	if got := controller.currentNode(); got != "A" {
		t.Fatalf("group now = %q, want restored A", got)
	}
	if got := pingpongStatus(t, m.Snapshot(), "B"); got != "dirty" {
		t.Fatalf("B pingpong = %q, want dirty", got)
	}

	// force=true bypasses the gate (the allowlist is still absolute).
	if _, err := m.ManualSwitch(context.Background(), "B", true); err != nil {
		t.Fatalf("forced ManualSwitch() error = %v", err)
	}
	if got := m.Snapshot().Status.DesiredNode; got != "B" {
		t.Fatalf("forced desired node = %q, want B", got)
	}
}

func TestManualNodeGoingDirtyExitsManualMode(t *testing.T) {
	m, controller, _, now := testManager(t)
	enablePingpong(m, controller, map[string]pingpong.Result{
		"A": {Status: pingpong.StatusPass},
		"B": {Status: pingpong.StatusPass},
	})
	_ = m.RunCycle(context.Background())
	if _, err := m.ManualSwitch(context.Background(), "B", false); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot().Status.Mode; got != "manual" {
		t.Fatalf("mode = %q, want manual", got)
	}
	*now = now.Add(301 * time.Second)
	// B's ping-pong verdict expires, the refresh marks it dirty and the
	// manager must fail over to the clean node A.
	enablePingpong(m, controller, map[string]pingpong.Result{
		"A": {Status: pingpong.StatusPass},
		"B": {Status: pingpong.StatusDirty, Detail: "location ban"},
	})
	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := m.Snapshot().Status
	if status.Mode != "auto" || status.DesiredNode != "A" {
		t.Fatalf("status = mode %q desired %q, want auto/A", status.Mode, status.DesiredNode)
	}
}

func TestPingpongCheckSingleAndSweep(t *testing.T) {
	m, controller, _, _ := testManager(t)
	enablePingpong(m, controller, map[string]pingpong.Result{
		"A": {Status: pingpong.StatusPass, Detail: "pong: pong", LatencyMS: 100},
		"B": {Status: pingpong.StatusDirty, Detail: "location ban", LatencyMS: 200},
	})
	_ = m.RunCycle(context.Background())

	report, err := m.PingpongCheck(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Node != "A" || report.Results[0].Status != "pass" {
		t.Fatalf("single report = %+v", report.Results)
	}

	report, err = m.PingpongCheck(context.Background(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("sweep report = %+v", report.Results)
	}
	if got := report.Snapshot.Status.DesiredNode; got != "A" {
		t.Fatalf("sweep landed on %q, want the only passing node A", got)
	}
	if got := controller.currentNode(); got != "A" {
		t.Fatalf("group now = %q, want A", got)
	}
}

func TestPingpongCheckRespectsDryRun(t *testing.T) {
	cfg := config.Default()
	cfg.AllowedNodes = []string{"A", "B"}
	controller := &fakeController{
		group:       mihomo.Group{Name: "PROXY", Type: "URLTest", Now: "A", Fixed: "A", All: []string{"A", "B"}},
		probes:      map[string]fakeProbe{"A": {delay: 500}, "B": {delay: 200}},
		probeCounts: make(map[string]int),
	}
	m := New(cfg, controller, newMemoryStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), true)
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	enablePingpong(m, controller, map[string]pingpong.Result{
		"A": {Status: pingpong.StatusPass},
		"B": {Status: pingpong.StatusPass},
	})

	// A forced sweep switches nodes, so dry-run must refuse it...
	if _, err := m.PingpongCheck(context.Background(), "", true); err == nil {
		t.Fatal("dry-run sweep unexpectedly succeeded")
	}
	// ...while testing the node traffic already exits through is fine.
	report, err := m.PingpongCheck(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Node != "A" || report.Results[0].Status != "pass" {
		t.Fatalf("dry-run single report = %+v", report.Results)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if len(controller.selections) != 0 {
		t.Fatalf("dry-run selections = %v, want none", controller.selections)
	}
}
