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

func (f *fakeController) selected() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.selections...)
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
