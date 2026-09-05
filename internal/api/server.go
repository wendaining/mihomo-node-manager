package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/local/mihomo-node-manager/internal/manager"
)

type Service interface {
	Snapshot() manager.Snapshot
	Healthy() bool
	ManualSwitch(context.Context, string, bool) (manager.Snapshot, error)
	ResumeAuto(context.Context) (manager.Snapshot, error)
}

type Server struct {
	service Service
	logger  *slog.Logger
	http    *http.Server
}

func New(listen string, service Service, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{service: service, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/v1/status", s.status)
	mux.HandleFunc("/v1/nodes", s.nodes)
	mux.HandleFunc("/v1/switch", s.manualSwitch)
	mux.HandleFunc("/v1/auto", s.resumeAuto)
	s.http = &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	return s
}

func (s *Server) ListenAndServe() error {
	s.logger.Info("api_listening", "address", s.http.Addr)
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	statusCode := http.StatusOK
	if !s.service.Healthy() {
		statusCode = http.StatusServiceUnavailable
	}
	writeJSON(w, statusCode, s.service.Snapshot().Status)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, s.service.Snapshot().Status)
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Nodes []manager.NodeStatus `json:"nodes"`
	}{Nodes: s.service.Snapshot().Nodes})
}

func (s *Server) manualSwitch(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var request struct {
		Node  string `json:"node"`
		Force bool   `json:"force"`
	}
	if err := decodeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if request.Node == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "node is required")
		return
	}
	snapshot, err := s.service.ManualSwitch(r.Context(), request.Node, request.Force)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot.Status)
}

func (s *Server) resumeAuto(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if r.Body != nil {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	}
	snapshot, err := s.service.ResumeAuto(r.Context())
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot.Status)
}

func (s *Server) writeOperationError(w http.ResponseWriter, err error) {
	var operationErr *manager.OperationError
	if !errors.As(err, &operationErr) {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	status := http.StatusInternalServerError
	switch operationErr.Code {
	case "node_not_allowed":
		status = http.StatusBadRequest
	case "node_not_present", "probe_failed", "dry_run":
		status = http.StatusConflict
	case "mihomo_unavailable", "selection_failed", "cycle_failed":
		status = http.StatusBadGateway
	}
	writeError(w, status, operationErr.Code, operationErr.Error())
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", fmt.Sprintf("use %s", method))
	return false
}

func decodeRequest(w http.ResponseWriter, r *http.Request, out any) error {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON object")
		}
		return err
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
