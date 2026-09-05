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
