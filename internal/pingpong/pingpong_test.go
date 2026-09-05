package pingpong

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct{ input, want string }{
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/v1/chat/completions"},
		{"http://127.0.0.1:8080/", "http://127.0.0.1:8080/v1/chat/completions"},
		{"http://127.0.0.1:8080/v1", "http://127.0.0.1:8080/v1/chat/completions"},
		{"https://cpa.example.com/v1/", "https://cpa.example.com/v1/chat/completions"},
		{"https://cpa.example.com/v1/chat/completions", "https://cpa.example.com/v1/chat/completions"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := NormalizeEndpoint(tt.input); got != tt.want {
			t.Fatalf("NormalizeEndpoint(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       Status
	}{
		{
			name:       "success",
			statusCode: 200,
			body:       `{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`,
			want:       StatusPass,
		},
		{
			name:       "empty 200 still passes",
			statusCode: 200,
			body:       `{"choices":[]}`,
			want:       StatusPass,
		},
		{
			name:       "gemini location ban",
			statusCode: 400,
			body:       `{'error': {'code': 400, 'message': 'User location is not supported for the API use.', 'status': 'FAILED_PRECONDITION'}}`,
			want:       StatusDirty,
		},
		{
			name:       "location ban newline formatted",
			statusCode: 400,
			body:       "Error code: 400 - {'error': {'code': 400, 'message': 'User location is not\n supported for the API use.', 'status': 'FAILED_PRECONDITION'}}",
			want:       StatusDirty,
		},
		{
			name:       "unrelated 400 is inconclusive",
			statusCode: 400,
			body:       `{"error":{"message":"max_tokens is too large"}}`,
			want:       StatusInconclusive,
		},
		{
			name:       "cpa oauth refresh outage",
			statusCode: 503,
			body:       `{'error': {'message': 'auth_unavailable: no auth available (providers=example, model=gemini-2.5-flash; last upstream error: Post "https://oauth2.googleapis.com/token": ...)', 'type': 'server_error', 'code': 'internal_server_error'}}`,
			want:       StatusInconclusive,
		},
		{
			name:       "rate limit is inconclusive",
			statusCode: 429,
			body:       `{"error":{"message":"rate limited"}}`,
			want:       StatusInconclusive,
		},
		{
			name:       "bad key is inconclusive",
			statusCode: 401,
			body:       `{"error":{"message":"invalid api key"}}`,
			want:       StatusInconclusive,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, detail := Classify(tt.statusCode, []byte(tt.body))
			if status != tt.want {
				t.Fatalf("Classify() = %q (detail %q), want %q", status, detail, tt.want)
			}
			if detail == "" {
				t.Fatal("Classify() returned an empty detail")
			}
		})
	}
}

func TestClassifyWithCustomRules(t *testing.T) {
	custom := Rules{Status: 503, BodyContains: []string{"Auth_Unavailable"}}

	// A response the built-in rules treat as inconclusive becomes dirty.
	status, detail := ClassifyWith(custom, 503, []byte(`{"error":{"message":"auth_unavailable: no auth available"}}`))
	if status != StatusDirty || detail == "" {
		t.Fatalf("ClassifyWith(custom, 503) = %q (%q), want dirty", status, detail)
	}

	// The custom rule stops covering the Gemini location ban.
	if status, _ := ClassifyWith(custom, http.StatusBadRequest, []byte(`{'error': {'message': 'User location is not supported'}}`)); status != StatusInconclusive {
		t.Fatalf("ClassifyWith(custom, 400) = %q, want inconclusive", status)
	}

	// Body matching stays case-insensitive.
	if status, _ := ClassifyWith(custom, 503, []byte("AUTH_UNAVAILABLE upstream")); status != StatusDirty {
		t.Fatalf("ClassifyWith(custom, uppercase body) = %q, want dirty", status)
	}

	// Built-in rules keep the Gemini ban dirty.
	if status, _ := ClassifyWith(DefaultRules(), 400, []byte(`{'error': {'status': 'FAILED_PRECONDITION'}}`)); status != StatusDirty {
		t.Fatalf("ClassifyWith(default, 400) = %q, want dirty", status)
	}
}

func TestTesterUsesConfiguredRules(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"auth_unavailable"}}`))
	}))
	defer server.Close()

	builtin := New(server.URL, "", "model", "ping", 16, 5)
	if result := builtin.Test(context.Background()); result.Status != StatusInconclusive {
		t.Fatalf("default rules: Test() = %+v, want inconclusive", result)
	}

	custom := NewWithRules(server.URL, "", "model", "ping", 16, 5, Rules{Status: 503, BodyContains: []string{"auth_unavailable"}})
	if result := custom.Test(context.Background()); result.Status != StatusDirty {
		t.Fatalf("custom rules: Test() = %+v, want dirty", result)
	}
}

func TestTesterSendsModelPromptAndKey(t *testing.T) {
	var gotPath, gotKey, gotModel, gotPrompt string
	var gotStream bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("Authorization")
		var request chatRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		gotModel, gotPrompt, gotStream = request.Model, request.Messages[0].Content, request.Stream
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer server.Close()

	tester := New(server.URL, "secret-key", "gemini-2.5-flash", "ping", 16, 5)
	result := tester.Test(context.Background())
	if result.Status != StatusPass {
		t.Fatalf("Test() = %+v, want pass", result)
	}
	if result.LatencyMS < 0 {
		t.Fatalf("negative latency %d", result.LatencyMS)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotKey != "Bearer secret-key" {
		t.Fatalf("authorization = %q", gotKey)
	}
	if gotModel != "gemini-2.5-flash" || gotPrompt != "ping" || gotStream {
		t.Fatalf("request = model %q prompt %q stream %v", gotModel, gotPrompt, gotStream)
	}
	if !strings.Contains(result.Detail, "pong") {
		t.Fatalf("detail = %q, want the model reply", result.Detail)
	}
}

func TestTesterOmitsAuthorizationWithoutKey(t *testing.T) {
	var gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	tester := New(server.URL, "", "model", "ping", 16, 5)
	if result := tester.Test(context.Background()); result.Status != StatusPass {
		t.Fatalf("Test() = %+v", result)
	}
	if gotKey != "" {
		t.Fatalf("authorization = %q, want absent", gotKey)
	}
}

func TestTesterNetworkFailureIsInconclusive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	server.Close()

	tester := New(server.URL, "", "model", "ping", 16, 1)
	result := tester.Test(context.Background())
	if result.Status != StatusInconclusive {
		t.Fatalf("Test() = %+v, want inconclusive", result)
	}
	if result.Detail == "" {
		t.Fatal("empty detail for a transport failure")
	}
}
