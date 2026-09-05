package mihomo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientProbeUnicodeAndSelect(t *testing.T) {
	const node = "node-01"
	const group = "PROXY"
	var mu sync.Mutex
	selected := "old"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer top-secret" {
			t.Errorf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/providers/proxies/subscription/"+node+"/healthcheck":
			if r.URL.Query().Get("url") != "https://www.gstatic.com/generate_204" || r.URL.Query().Get("expected") != "204" || r.URL.Query().Get("timeout") != "5000" {
				t.Errorf("unexpected probe query: %v", r.URL.Query())
			}
			_ = json.NewEncoder(w).Encode(map[string]int{"delay": 123})
		case r.Method == http.MethodGet && r.URL.Path == "/providers/proxies":
			_ = json.NewEncoder(w).Encode(map[string]any{"providers": map[string]any{
				"subscription": map[string]any{"proxies": []map[string]string{{"name": node}}},
			}})
		case r.Method == http.MethodPut && r.URL.Path == "/proxies/"+group:
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			mu.Lock()
			selected = body.Name
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/proxies/"+group:
			mu.Lock()
			current := selected
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(Group{Name: group, Type: "URLTest", Now: current, Fixed: current, All: []string{node}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "top-secret", 2*time.Second)
	providers, err := client.Providers(context.Background())
	if err != nil || providers[node] != "subscription" {
		t.Fatalf("Providers() = %v, %v", providers, err)
	}
	delay, err := client.Probe(context.Background(), "subscription", node, "https://www.gstatic.com/generate_204", "204", 5*time.Second)
	if err != nil || delay != 123 {
		t.Fatalf("Probe() = %d, %v", delay, err)
	}
	actual, err := client.Select(context.Background(), group, node)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if actual.Now != node || actual.Fixed != node {
		t.Fatalf("Select() actual = %+v", actual)
	}
}

func TestClientReportsHTTPAndVerificationErrors(t *testing.T) {
	t.Run("non-2xx probe", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "probe failed", http.StatusGatewayTimeout)
		}))
		defer server.Close()
		client := NewClient(server.URL, "", time.Second)
		_, err := client.Probe(context.Background(), "subscription", "node", "https://example.test", "204", time.Second)
		if err == nil || !strings.Contains(err.Error(), "504") {
			t.Fatalf("Probe() error = %v", err)
		}
	})

	t.Run("selection not fixed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(Group{Name: "PROXY", Type: "URLTest", Now: "node", Fixed: "", All: []string{"node"}})
		}))
		defer server.Close()
		client := NewClient(server.URL, "", time.Second)
		_, err := client.Select(context.Background(), "PROXY", "node")
		if err == nil || !strings.Contains(err.Error(), "fixed") {
			t.Fatalf("Select() error = %v", err)
		}
	})

	t.Run("PUT failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "cannot select", http.StatusInternalServerError)
		}))
		defer server.Close()
		client := NewClient(server.URL, "", time.Second)
		_, err := client.Select(context.Background(), "PROXY", "node")
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("Select() error = %v", err)
		}
	})

	t.Run("request timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]int{"delay": 10})
		}))
		defer server.Close()
		client := NewClient(server.URL, "", 20*time.Millisecond)
		_, err := client.Probe(context.Background(), "subscription", "node", "https://example.test", "204", time.Second)
		if err == nil {
			t.Fatal("Probe() unexpectedly succeeded")
		}
	})
}

func TestCloseGroupConnectionsFiltersByChain(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/connections":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"connections": []map[string]any{
					{"id": "conn-1", "chains": []string{"node-01", "PROXY"}},
					{"id": "conn-2", "chains": []string{"DIRECT"}},
					{"id": "conn-3", "chains": []string{"OTHER-GROUP"}},
					{"id": "conn-4", "chains": []string{"node-06", "PROXY"}},
				},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/connections/"):
			mu.Lock()
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/connections/"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "", time.Second)
	closed, err := client.CloseGroupConnections(context.Background(), "PROXY")
	if err != nil {
		t.Fatalf("CloseGroupConnections() error = %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2", closed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 2 || deleted[0] != "conn-1" || deleted[1] != "conn-4" {
		t.Fatalf("deleted = %v", deleted)
	}
}

func TestPathEscapeRoundTrip(t *testing.T) {
	name := "EU / 01"
	escaped := url.PathEscape(name)
	if unescaped, err := url.PathUnescape(escaped); err != nil || unescaped != name {
		t.Fatalf("path round trip = %q, %v", unescaped, err)
	}
}
