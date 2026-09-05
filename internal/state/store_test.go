package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	store := NewStore(path)
	want := New()
	want.DesiredNode = "node-a"
	want.Nodes["node-a"] = &Node{Name: "node-a", Available: true, LastCheckedAt: time.Now().UTC().Truncate(time.Second)}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.DesiredNode != want.DesiredNode || !got.Nodes["node-a"].Available {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestStoreCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewStore(path).Load()
	if err == nil || !strings.Contains(err.Error(), "decode state") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestStoreLoadsLegacyVersionOneState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{
	  "version": 1,
	  "desired_node": "node-a",
	  "nodes": {"node-a": {"name": "node-a", "available": true}}
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.DesiredNode != "node-a" || !got.Nodes["node-a"].Available {
		t.Fatalf("legacy state = %+v", got)
	}
	if got.Nodes["node-a"].PingpongStatus != "" {
		t.Fatalf("legacy node should start with unknown pingpong status, got %q", got.Nodes["node-a"].PingpongStatus)
	}
}

func TestStoreRejectsFutureVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version": 99, "nodes": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewStore(path).Load()
	if err == nil || !strings.Contains(err.Error(), "unsupported state version 99") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestStoreRoundTripsPingpongFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)
	checked := time.Now().UTC().Truncate(time.Second)
	value := New()
	value.Nodes["node-a"] = &Node{
		Name:              "node-a",
		PingpongStatus:    "dirty",
		PingpongCheckedAt: checked,
		PingpongLatencyMS: 512,
		PingpongDetail:    "HTTP 400: User location is not supported",
	}
	if err := store.Save(value); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	node := got.Nodes["node-a"]
	if node.PingpongStatus != "dirty" || !node.PingpongCheckedAt.Equal(checked) || node.PingpongLatencyMS != 512 {
		t.Fatalf("pingpong fields did not round trip: %+v", node)
	}
}
