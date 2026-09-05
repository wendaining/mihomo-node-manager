// Package pingpong performs one minimal OpenAI-compatible chat completion
// against a CPA (cli-proxy-api) style Gemini reverse proxy. The CPA's upstream
// Google traffic egresses through whatever node Mihomo's policy group currently
// selects, so the verdict reflects exactly the node that was selected while the
// request was made. Callers switch the group before calling Test.
package pingpong

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Status string

const (
	// StatusPass means the CPA answered with a completion: the egress node is
	// accepted by Google.
	StatusPass Status = "pass"
	// StatusDirty means Google rejected the request with the location ban
	// (HTTP 400, "User location is not supported"): the egress node triggers
	// the risk control.
	StatusDirty Status = "dirty"
	// StatusInconclusive covers everything else - CPA auth refresh (503
	// auth_unavailable), rate limits, outages, timeouts. It says nothing
	// about the node and must never be held against it.
	StatusInconclusive Status = "inconclusive"
)

type Result struct {
	Status    Status
	LatencyMS int
	Detail    string
}

type Tester struct {
	endpoint  string
	apiKey    string
	model     string
	prompt    string
	maxTokens int
	client    *http.Client
}

// NormalizeEndpoint turns a user supplied CPA base URL into the full chat
// completions endpoint. "http://host:port", "http://host:port/v1" and a full
// ".../v1/chat/completions" URL are all accepted.
func NormalizeEndpoint(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	switch {
	case strings.HasSuffix(base, "/chat/completions"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/chat/completions"
	default:
		return base + "/v1/chat/completions"
	}
}

func New(endpoint, apiKey, model, prompt string, maxTokens, timeoutSeconds int) *Tester {
	return &Tester{
		endpoint:  NormalizeEndpoint(endpoint),
		apiKey:    strings.TrimSpace(apiKey),
		model:     model,
		prompt:    prompt,
		maxTokens: maxTokens,
		client:    &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second},
	}
}

// Test performs the ping-pong round trip and measures its wall time.
func (t *Tester) Test(ctx context.Context) Result {
	start := time.Now()
	result := t.do(ctx)
	result.LatencyMS = int(time.Since(start).Milliseconds())
	return result
}

func (t *Tester) do(ctx context.Context) Result {
	payload, err := json.Marshal(chatRequest{
		Model:     t.model,
		Messages:  []chatMessage{{Role: "user", Content: t.prompt}},
		MaxTokens: t.maxTokens,
		Stream:    false,
	})
	if err != nil {
		return Result{Status: StatusInconclusive, Detail: fmt.Sprintf("build request: %v", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(payload))
	if err != nil {
		return Result{Status: StatusInconclusive, Detail: fmt.Sprintf("build request: %v", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return Result{Status: StatusInconclusive, Detail: fmt.Sprintf("request failed: %v", err)}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if readErr != nil {
		return Result{Status: StatusInconclusive, Detail: fmt.Sprintf("read response: %v", readErr)}
	}
	status, detail := Classify(resp.StatusCode, body)
	return Result{Status: status, Detail: detail}
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
	Stream    bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Classify maps a CPA response to a ping-pong verdict. Only the Gemini
// location ban (HTTP 400) proves a node is dirty. Everything else - including
// the 503 auth_unavailable responses the CPA emits while refreshing its OAuth
// credentials - is inconclusive, so a CPA side outage can never cause nodes to
// be marked dirty or trigger failovers.
func Classify(statusCode int, body []byte) (Status, string) {
	snippet := collapseWhitespace(string(body))
	if statusCode >= 200 && statusCode < 300 {
		return StatusPass, "pong: " + extractContent(body)
	}
	lower := strings.ToLower(snippet)
	if statusCode == http.StatusBadRequest &&
		(strings.Contains(lower, "user location is not supported") || strings.Contains(lower, "failed_precondition")) {
		return StatusDirty, snippet
	}
	return StatusInconclusive, fmt.Sprintf("HTTP %d: %s", statusCode, snippet)
}

func extractContent(body []byte) string {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Choices) == 0 {
		return "ok"
	}
	if content := strings.TrimSpace(parsed.Choices[0].Message.Content); content != "" {
		return truncate(content, 80)
	}
	return "ok"
}

func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
