package dotenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseHandlesCommentsQuotesAndExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := `
# comment line
   # indented comment

PLAIN=value
export EXPORTED=1
QUOTED="hello world"
SINGLE='a=b'
EMPTY=
SPACED = spaced value
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := [][2]string{
		{"PLAIN", "value"},
		{"EXPORTED", "1"},
		{"QUOTED", "hello world"},
		{"SINGLE", "a=b"},
		{"EMPTY", ""},
		{"SPACED", "spaced value"},
	}
	if len(entries) != len(want) {
		t.Fatalf("Parse() = %v, want %v", entries, want)
	}
	for i, entry := range want {
		if entries[i] != entry {
			t.Fatalf("entry %d = %v, want %v", i, entries[i], entry)
		}
	}
}

func TestParseRejectsMalformedLines(t *testing.T) {
	for _, line := range []string{"NOEQUALS", "1BAD=x", "BAD NAME=x", "="} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(path); err == nil {
			t.Fatalf("Parse(%q) unexpectedly succeeded", line)
		}
	}
}

func TestLoadSetsMissingVariablesAndKeepsExistingOnes(t *testing.T) {
	t.Setenv("EXISTING", "process-value")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("EXISTING=file-value\nNEW=new-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := os.Getenv("EXISTING"); got != "process-value" {
		t.Fatalf("EXISTING = %q, want process value to win", got)
	}
	if got := os.Getenv("NEW"); got != "new-value" {
		t.Fatalf("NEW = %q", got)
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	if err := Load(filepath.Join(t.TempDir(), "absent.env")); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}
