package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/local/mihomo-node-manager/internal/dotenv"
)

type Config struct {
	StateFile    string         `toml:"state_file"`
	AllowedNodes []string       `toml:"allowed_nodes"`
	Mihomo       MihomoConfig   `toml:"mihomo"`
	Probe        ProbeConfig    `toml:"probe"`
	Policy       PolicyConfig   `toml:"policy"`
	Pingpong     PingpongConfig `toml:"pingpong"`
	API          APIConfig      `toml:"api"`
	Logging      LoggingConfig  `toml:"logging"`
}

type MihomoConfig struct {
	BaseURL          string `toml:"base_url"`
	Group            string `toml:"group"`
	SecretFile       string `toml:"secret_file"`
	RequestTimeoutMS int    `toml:"request_timeout_ms"`
}

type ProbeConfig struct {
	URL             string `toml:"url"`
	ExpectedStatus  string `toml:"expected_status"`
	TimeoutMS       int    `toml:"timeout_ms"`
	IntervalSeconds int    `toml:"interval_seconds"`
	Concurrency     int    `toml:"concurrency"`
	HistorySize     int    `toml:"history_size"`
}

type PolicyConfig struct {
	EWMAAlpha             float64 `toml:"ewma_alpha"`
	FailureThreshold      int     `toml:"failure_threshold"`
	RecoveryThreshold     int     `toml:"recovery_threshold"`
	MinDwellSeconds       int     `toml:"min_dwell_seconds"`
	ImprovementRatio      float64 `toml:"improvement_ratio"`
	ImprovementMS         int     `toml:"improvement_ms"`
	BetterRounds          int     `toml:"better_rounds"`
	ManualOverrideSeconds int     `toml:"manual_override_seconds"`
}

// PingpongConfig drives the CPA Gemini ping-pong probe. The endpoint, API key
// and model come from the environment (optionally seeded by env_file) so that
// secrets stay out of config.toml.
type PingpongConfig struct {
	EnvFile                string `toml:"env_file"`
	Enabled                bool   `toml:"enabled"`
	RefreshIntervalSeconds int    `toml:"refresh_interval_seconds"`
	FailTTLSeconds         int    `toml:"fail_ttl_seconds"`
	TimeoutSeconds         int    `toml:"timeout_seconds"`
	MaxTokens              int    `toml:"max_tokens"`
	Prompt                 string `toml:"prompt"`
	SafeNode               string `toml:"safe_node"`
	CloseConnsOnSwitch     bool   `toml:"close_conns_on_switch"`

	// Resolved from the CPA_* environment variables; never set from TOML.
	BaseURL string `toml:"-"`
	APIKey  string `toml:"-"`
	Model   string `toml:"-"`
}

// Active reports whether the ping-pong probe should run. Both the CPA base URL
// and the model are required; anything less disables the feature instead of
// producing broken probes.
func (p PingpongConfig) Active() bool {
	return p.Enabled && p.BaseURL != "" && p.Model != ""
}

type APIConfig struct {
	Listen string `toml:"listen"`
}

type LoggingConfig struct {
	Level string `toml:"level"`
}

func Default() Config {
	return Config{
		StateFile: "/var/lib/mihomo-node-manager/state.json",
		Mihomo: MihomoConfig{
			BaseURL:          "http://127.0.0.1:9090",
			Group:            "PROXY",
			RequestTimeoutMS: 6000,
		},
		Probe: ProbeConfig{
			URL:             "https://www.gstatic.com/generate_204",
			ExpectedStatus:  "204",
			TimeoutMS:       5000,
			IntervalSeconds: 60,
			Concurrency:     4,
			HistorySize:     10,
		},
		Policy: PolicyConfig{
			EWMAAlpha:             0.35,
			FailureThreshold:      2,
			RecoveryThreshold:     2,
			MinDwellSeconds:       600,
			ImprovementRatio:      0.20,
			ImprovementMS:         100,
			BetterRounds:          3,
			ManualOverrideSeconds: 1800,
		},
		Pingpong: PingpongConfig{
			// Relative paths resolve against the process working directory.
			// The systemd unit sets WorkingDirectory=/etc/mihomo-node-manager,
			// so the same default works locally (repo ./.env) and on the
			// server (/etc/mihomo-node-manager/.env).
			EnvFile:                ".env",
			Enabled:                true,
			RefreshIntervalSeconds: 300,
			FailTTLSeconds:         1800,
			TimeoutSeconds:         20,
			MaxTokens:              16,
			Prompt:                 "ping",
			SafeNode:               "",
			CloseConnsOnSwitch:     true,
		},
		API:     APIConfig{Listen: "127.0.0.1:9123"},
		Logging: LoggingConfig{Level: "info"},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		parts := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			parts = append(parts, key.String())
		}
		return Config{}, fmt.Errorf("unknown config keys: %s", strings.Join(parts, ", "))
	}
	if err := cfg.Pingpong.loadEnvironment(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	cfg.Mihomo.BaseURL = strings.TrimRight(cfg.Mihomo.BaseURL, "/")
	return cfg, nil
}

// loadEnvironment seeds the process environment from pingpong.env_file and
// then resolves the CPA_* variables. Variables that already exist in the
// environment win over the file. A missing env_file is fine; a malformed one
// is a hard error.
func (p *PingpongConfig) loadEnvironment() error {
	if p.EnvFile != "" {
		if err := dotenv.Load(p.EnvFile); err != nil {
			return fmt.Errorf("load pingpong env_file: %w", err)
		}
	}
	p.BaseURL = strings.TrimSpace(os.Getenv("CPA_BASE_URL"))
	p.APIKey = strings.TrimSpace(os.Getenv("CPA_API_KEY"))
	p.Model = strings.TrimSpace(os.Getenv("CPA_MODEL"))
	return nil
}

func (c Config) Validate() error {
	var errs []error
	if c.StateFile == "" {
		errs = append(errs, errors.New("state_file is required"))
	}
	if len(c.AllowedNodes) == 0 {
		errs = append(errs, errors.New("allowed_nodes must not be empty"))
	}
	seen := make(map[string]struct{}, len(c.AllowedNodes))
	for i, node := range c.AllowedNodes {
		if strings.TrimSpace(node) == "" {
			errs = append(errs, fmt.Errorf("allowed_nodes[%d] is empty", i))
			continue
		}
		if _, ok := seen[node]; ok {
			errs = append(errs, fmt.Errorf("allowed_nodes contains duplicate %q", node))
		}
		seen[node] = struct{}{}
	}
	parsed, err := url.Parse(c.Mihomo.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		errs = append(errs, errors.New("mihomo.base_url must be an http or https URL"))
	}
	if c.Mihomo.Group == "" {
		errs = append(errs, errors.New("mihomo.group is required"))
	}
	if c.Mihomo.RequestTimeoutMS <= 0 {
		errs = append(errs, errors.New("mihomo.request_timeout_ms must be positive"))
	}
	probeURL, err := url.Parse(c.Probe.URL)
	if err != nil || probeURL.Scheme == "" || probeURL.Host == "" {
		errs = append(errs, errors.New("probe.url must be an absolute URL"))
	}
	if c.Probe.ExpectedStatus == "" {
		errs = append(errs, errors.New("probe.expected_status is required"))
	}
	if c.Probe.TimeoutMS <= 0 || c.Probe.IntervalSeconds <= 0 || c.Probe.Concurrency <= 0 || c.Probe.HistorySize <= 0 {
		errs = append(errs, errors.New("probe timeout, interval, concurrency and history_size must be positive"))
	}
	if c.Policy.EWMAAlpha <= 0 || c.Policy.EWMAAlpha > 1 {
		errs = append(errs, errors.New("policy.ewma_alpha must be in (0, 1]"))
	}
	if c.Policy.FailureThreshold <= 0 || c.Policy.RecoveryThreshold <= 0 || c.Policy.BetterRounds <= 0 {
		errs = append(errs, errors.New("policy thresholds and better_rounds must be positive"))
	}
	if c.Policy.MinDwellSeconds < 0 || c.Policy.ImprovementMS < 0 || c.Policy.ManualOverrideSeconds <= 0 {
		errs = append(errs, errors.New("policy durations and improvement_ms are invalid"))
	}
	if c.Policy.ImprovementRatio < 0 || c.Policy.ImprovementRatio >= 1 {
		errs = append(errs, errors.New("policy.improvement_ratio must be in [0, 1)"))
	}
	if c.Pingpong.Enabled && (c.Pingpong.BaseURL == "") != (c.Pingpong.Model == "") {
		errs = append(errs, errors.New("pingpong requires both CPA_BASE_URL and CPA_MODEL (set them in the env_file or the process environment)"))
	}
	if c.Pingpong.BaseURL != "" {
		parsed, err := url.Parse(c.Pingpong.BaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			errs = append(errs, errors.New("CPA_BASE_URL must be an absolute http or https URL"))
		}
	}
	if c.Pingpong.Active() {
		if c.Pingpong.RefreshIntervalSeconds <= 0 || c.Pingpong.FailTTLSeconds <= 0 || c.Pingpong.TimeoutSeconds <= 0 || c.Pingpong.MaxTokens <= 0 {
			errs = append(errs, errors.New("pingpong refresh_interval_seconds, fail_ttl_seconds, timeout_seconds and max_tokens must be positive"))
		}
		if strings.TrimSpace(c.Pingpong.Prompt) == "" {
			errs = append(errs, errors.New("pingpong.prompt must not be empty"))
		}
		if c.Pingpong.SafeNode != "" && !containsNode(c.AllowedNodes, c.Pingpong.SafeNode) {
			errs = append(errs, errors.New("pingpong.safe_node must be one of allowed_nodes"))
		}
	}
	host, _, err := net.SplitHostPort(c.API.Listen)
	if err != nil {
		errs = append(errs, fmt.Errorf("api.listen: %w", err))
	} else if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			errs = append(errs, errors.New("api.listen must use a loopback address"))
		}
	}
	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, errors.New("logging.level must be debug, info, warn or error"))
	}
	return errors.Join(errs...)
}

func containsNode(nodes []string, name string) bool {
	for _, node := range nodes {
		if node == name {
			return true
		}
	}
	return false
}
