package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadExampleConfig(t *testing.T) {
	path := filepath.Join("..", "..", "config", "config.toml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := len(cfg.AllowedNodes), 6; got != want {
		t.Fatalf("allowed node count = %d, want %d", got, want)
	}
	if cfg.API.Listen != "127.0.0.1:9123" {
		t.Fatalf("unexpected API listen address %q", cfg.API.Listen)
	}
}

func TestLoadRejectsUnknownAndUnsafeValues(t *testing.T) {
	tests := []struct {
		name string
		edit func(string) string
		want string
	}{
		{
			name: "unknown key",
			edit: func(text string) string { return text + "\nunknown_key = true\n" },
			want: "unknown config keys",
		},
		{
			name: "non-loopback API",
			edit: func(text string) string { return strings.Replace(text, "127.0.0.1:9123", "0.0.0.0:9123", 1) },
			want: "loopback",
		},
		{
			name: "duplicate node",
			edit: func(text string) string {
				return strings.Replace(text, "  \"🇯🇵 日本 02\",", "  \"🇯🇵 日本 01\",", 1)
			},
			want: "duplicate",
		},
	}
	original, err := os.ReadFile(filepath.Join("..", "..", "config", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.edit(string(original))), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
