package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/local/mihomo-node-manager/internal/manager"
)

type fakeService struct {
	snapshot  manager.Snapshot
	healthy   bool
	switchErr error
	autoErr   error
	pingpongR manager.PingpongReport
	pingpongE error
	node      string
	force     bool
	pingpongN string
	pingpongF bool
}

func (f *fakeService) Snapshot() manager.Snapshot { return f.snapshot }
func (f *fakeService) Healthy() bool              { return f.healthy }
func (f *fakeService) ManualSwitch(_ context.Context, node string, force bool) (manager.Snapshot, error) {
	f.node, f.force = node, force
	return f.snapshot, f.switchErr
}
func (f *fakeService) ResumeAuto(context.Context) (manager.Snapshot, error) {
	return f.snapshot, f.autoErr
}
func (f *fakeService) PingpongCheck(_ context.Context, node string, force bool) (manager.PingpongReport, error) {
	f.pingpongN, f.pingpongF = node, force
	return f.pingpongR, f.pingpongE
}

func testHandler(service *fakeService) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New("127.0.0.1:0", service, logger).http.Handler
}

func TestHealthAndStatusEndpoints(t *testing.T) {
	service := &fakeService{snapshot: manager.Snapshot{Status: manager.Status{Status: "degraded", Group: "PROXY"}}}
	handler := testHandler(service)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("health status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}

	service.healthy = true
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "PROXY") {
		t.Fatalf("status response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestManualSwitchValidationAndErrors(t *testing.T) {
	service := &fakeService{snapshot: manager.Snapshot{Status: manager.Status{Status: "ok"}}}
	handler := testHandler(service)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/switch", strings.NewReader(`{"node":"B","force":true}`))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || service.node != "B" || !service.force {
		t.Fatalf("switch = status %d node=%q force=%v body=%s", recorder.Code, service.node, service.force, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/switch", strings.NewReader(`{"node":"B","extra":1}`))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", recorder.Code)
	}

	service.switchErr = &manager.OperationError{Code: "probe_failed", Err: errors.New("timeout")}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/switch", strings.NewReader(`{"node":"B"}`))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("probe failure status = %d", recorder.Code)
	}
	var response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Error.Code != "probe_failed" {
		t.Fatalf("error response = %s, %v", recorder.Body.String(), err)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	handler := testHandler(&fakeService{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/v1/nodes", nil))
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("response = %d allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}
}

func TestPingpongCheckEndpoint(t *testing.T) {
	service := &fakeService{
		snapshot: manager.Snapshot{Status: manager.Status{Status: "ok"}},
		pingpongR: manager.PingpongReport{
			Results: []manager.PingpongCheckResult{{Node: "A", Status: "pass", LatencyMS: 123, Detail: "pong: pong"}},
		},
	}
	handler := testHandler(service)

	// Empty body means "test the current node".
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/pingpong", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("empty body status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if service.pingpongN != "" || service.pingpongF {
		t.Fatalf("node=%q force=%v, want defaults", service.pingpongN, service.pingpongF)
	}
	if !strings.Contains(recorder.Body.String(), `"status":"pass"`) {
		t.Fatalf("report body = %s", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/pingpong", strings.NewReader(`{"node":"B","force":true}`)))
	if recorder.Code != http.StatusOK || service.pingpongN != "B" || !service.pingpongF {
		t.Fatalf("force request = %d node=%q force=%v", recorder.Code, service.pingpongN, service.pingpongF)
	}

	service.pingpongE = &manager.OperationError{Code: "pingpong_disabled", Err: errors.New("not configured")}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/pingpong", nil))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("disabled status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/pingpong", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", recorder.Code)
	}
}
