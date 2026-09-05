package state

import "time"

// Version 2 adds the per-node pingpong_* fields. Version 1 files (written by
// 0.1.0) still load; their pingpong verdicts simply start out unknown.
const CurrentVersion = 2

type Sample struct {
	At      time.Time `json:"at"`
	Success bool      `json:"success"`
	DelayMS int       `json:"delay_ms,omitempty"`
}

// Ping-pong verdicts stored on each node: "pass", "dirty" or "inconclusive".
// An empty PingpongStatus means the node has never been ping-pong tested.
type Node struct {
	Name                 string    `json:"name"`
	Present              bool      `json:"present"`
	Known                bool      `json:"known"`
	Available            bool      `json:"available"`
	LastProbeSuccess     bool      `json:"last_probe_success"`
	LastDelayMS          int       `json:"last_delay_ms,omitempty"`
	EWMADelayMS          float64   `json:"ewma_delay_ms,omitempty"`
	ConsecutiveSuccesses int       `json:"consecutive_successes"`
	ConsecutiveFailures  int       `json:"consecutive_failures"`
	LastCheckedAt        time.Time `json:"last_checked_at,omitempty"`
	LastError            string    `json:"last_error,omitempty"`
	Samples              []Sample  `json:"samples,omitempty"`
	PingpongStatus       string    `json:"pingpong_status,omitempty"`
	PingpongCheckedAt    time.Time `json:"pingpong_checked_at,omitempty"`
	PingpongLatencyMS    int       `json:"pingpong_latency_ms,omitempty"`
	PingpongDetail       string    `json:"pingpong_detail,omitempty"`
}

type Persisted struct {
	Version         int              `json:"version"`
	DesiredNode     string           `json:"desired_node,omitempty"`
	SelectedSince   time.Time        `json:"selected_since,omitempty"`
	ManualNode      string           `json:"manual_node,omitempty"`
	ManualUntil     time.Time        `json:"manual_until,omitempty"`
	BetterCandidate string           `json:"better_candidate,omitempty"`
	BetterRounds    int              `json:"better_rounds,omitempty"`
	Nodes           map[string]*Node `json:"nodes"`
}

func New() Persisted {
	return Persisted{Version: CurrentVersion, Nodes: make(map[string]*Node)}
}
