package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadExampleConfig(t *testing.T) {
	path := filepath.Join("..", "..", "config", "config.example.toml")
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
	if !cfg.Pingpong.Enabled {
		t.Fatal("pingpong should be enabled by default in the example config")
	}
	if cfg.Pingpong.Active() {
		t.Fatal("pingpong must stay inactive until CPA_BASE_URL and CPA_MODEL are configured")
	}
	if cfg.Pingpong.SafeNode != "" {
		t.Fatalf("safe_node = %q, want the empty default", cfg.Pingpong.SafeNode)
	}
}

func TestPingpongEnvironmentLoading(t *testing.T) {
	repoConfig, err := filepath.Abs(filepath.Join("..", "..", "config", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	clearCPAEnv(t)

	t.Run("env file enables the probe", func(t *testing.T) {
		clearCPAEnv(t)
		t.Chdir(t.TempDir())
		if err := os.WriteFile(".env", []byte("CPA_BASE_URL=http://127.0.0.1:8317\nCPA_API_KEY=secret\nCPA_MODEL=gemini-3.8-flash-high\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(repoConfig)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !cfg.Pingpong.Active() {
			t.Fatal("pingpong should be active with CPA_BASE_URL and CPA_MODEL set")
		}
		if cfg.Pingpong.BaseURL != "http://127.0.0.1:8317" || cfg.Pingpong.Model != "gemini-3.8-flash-high" || cfg.Pingpong.APIKey != "secret" {
			t.Fatalf("resolved pingpong config = %+v", cfg.Pingpong)
		}
	})

	t.Run("process environment wins over the file", func(t *testing.T) {
		clearCPAEnv(t)
		t.Chdir(t.TempDir())
		if err := os.WriteFile(".env", []byte("CPA_BASE_URL=http://from-file:1\nCPA_MODEL=from-file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CPA_BASE_URL", "http://from-env:2")
		cfg, err := Load(repoConfig)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Pingpong.BaseURL != "http://from-env:2" || cfg.Pingpong.Model != "from-file" {
			t.Fatalf("resolved pingpong config = %+v", cfg.Pingpong)
		}
	})

	t.Run("half configured is a hard error", func(t *testing.T) {
		clearCPAEnv(t)
		t.Setenv("CPA_BASE_URL", "http://127.0.0.1:8317")
		if _, err := Load(repoConfig); err == nil || !strings.Contains(err.Error(), "CPA_BASE_URL") {
			t.Fatalf("Load() error = %v, want a CPA_BASE_URL/CPA_MODEL pairing error", err)
		}
	})

	t.Run("malformed env file is a hard error", func(t *testing.T) {
		clearCPAEnv(t)
		t.Chdir(t.TempDir())
		if err := os.WriteFile(".env", []byte("this line has no equals\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(repoConfig); err == nil || !strings.Contains(err.Error(), "env_file") {
			t.Fatalf("Load() error = %v, want an env_file error", err)
		}
	})
}

// clearCPAEnv removes the CPA_* variables for the duration of a test and
// restores them afterwards. dotenv.Load mutates the real process environment,
// so without this a .env written by one test would leak into the next.
func clearCPAEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"CPA_BASE_URL", "CPA_API_KEY", "CPA_MODEL"} {
		key := key
		if previous, existed := os.LookupEnv(key); existed {
			t.Cleanup(func() { _ = os.Setenv(key, previous) })
		}
		_ = os.Unsetenv(key)
	}
}

func TestPingpongValidation(t *testing.T) {
	clearCPAEnv(t)
	original, err := os.ReadFile(filepath.Join("..", "..", "config", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	baseText := string(original)
	load := func(text string) error {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		return err
	}

	t.Run("safe_node must be allowed", func(t *testing.T) {
		clearCPAEnv(t)
		// The safe_node check only applies while the probe is active.
		t.Chdir(t.TempDir())
		if err := os.WriteFile(".env", []byte("CPA_BASE_URL=http://127.0.0.1:8317\nCPA_MODEL=m\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		text := strings.Replace(baseText, `safe_node = ""`, `safe_node = "Evil Node"`, 1)
		err := load(text)
		if err == nil || !strings.Contains(err.Error(), "safe_node") {
			t.Fatalf("Load() error = %v, want a safe_node error", err)
		}
	})

	t.Run("CPA base URL must be absolute", func(t *testing.T) {
		clearCPAEnv(t)
		t.Chdir(t.TempDir())
		if err := os.WriteFile(".env", []byte("CPA_BASE_URL=not-a-url\nCPA_MODEL=m\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := load(baseText)
		if err == nil || !strings.Contains(err.Error(), "CPA_BASE_URL") {
			t.Fatalf("Load() error = %v, want a CPA_BASE_URL error", err)
		}
	})
}

func TestPingpongDirtyMatchValidation(t *testing.T) {
	clearCPAEnv(t)
	original, err := os.ReadFile(filepath.Join("..", "..", "config", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	baseText := string(original)
	load := func(text string) error {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		return err
	}

	t.Run("custom rules are accepted even while the probe is inactive", func(t *testing.T) {
		text := baseText + "\n[pingpong.dirty_match]\nstatus = 503\nbody_contains = [\"auth_unavailable\"]\n"
		if err := load(text); err != nil {
			t.Fatalf("Load() error = %v", err)
		}
	})

	t.Run("status outside 400-599 is rejected", func(t *testing.T) {
		text := baseText + "\n[pingpong.dirty_match]\nstatus = 200\nbody_contains = [\"x\"]\n"
		err := load(text)
		if err == nil || !strings.Contains(err.Error(), "dirty_match.status") {
			t.Fatalf("Load() error = %v, want a dirty_match.status error", err)
		}
	})

	t.Run("empty body_contains is rejected", func(t *testing.T) {
		text := baseText + "\n[pingpong.dirty_match]\nstatus = 400\nbody_contains = []\n"
		err := load(text)
		if err == nil || !strings.Contains(err.Error(), "body_contains") {
			t.Fatalf("Load() error = %v, want a body_contains error", err)
		}
	})

	t.Run("blank body_contains entry is rejected", func(t *testing.T) {
		text := baseText + "\n[pingpong.dirty_match]\nstatus = 400\nbody_contains = [\" \"]\n"
		err := load(text)
		if err == nil || !strings.Contains(err.Error(), "body_contains") {
			t.Fatalf("Load() error = %v, want a body_contains error", err)
		}
	})
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
				return strings.Replace(text, "  \"node-02\",", "  \"node-01\",", 1)
			},
			want: "duplicate",
		},
	}
	original, err := os.ReadFile(filepath.Join("..", "..", "config", "config.example.toml"))
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
