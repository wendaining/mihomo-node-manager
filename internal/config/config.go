package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	StateFile    string        `toml:"state_file"`
	AllowedNodes []string      `toml:"allowed_nodes"`
	Mihomo       MihomoConfig  `toml:"mihomo"`
	Probe        ProbeConfig   `toml:"probe"`
	Policy       PolicyConfig  `toml:"policy"`
	API          APIConfig     `toml:"api"`
	Logging      LoggingConfig `toml:"logging"`
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
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	cfg.Mihomo.BaseURL = strings.TrimRight(cfg.Mihomo.BaseURL, "/")
	return cfg, nil
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
