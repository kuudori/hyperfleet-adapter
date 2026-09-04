package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// CheckStatus represents the status of a single health check.
type CheckStatus string

const (
	// CheckOK indicates the check passed.
	CheckOK CheckStatus = "ok"
	// CheckError indicates the check failed.
	CheckError CheckStatus = "error"
)

// HealthResponse represents the JSON response for /healthz endpoint.
type HealthResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// ReadyResponse represents the JSON response for /readyz endpoint per HyperFleet standard.
type ReadyResponse struct {
	Checks  map[string]CheckStatus `json:"checks,omitempty"`
	Status  string                 `json:"status"`
	Message string                 `json:"message,omitempty"`
}

// Server provides HTTP health check endpoints.
type Server struct {
	server     *http.Server
	checks     map[string]CheckStatus
	port       string
	component  string
	configYAML []byte // set only when debug_config is true
	mu         sync.RWMutex
	// shuttingDown is an atomic flag that indicates the server is shutting down.
	// When true, /readyz immediately returns 503 regardless of other checks.
	// This follows the HyperFleet Graceful Shutdown Standard.
	shuttingDown atomic.Bool
}

// NewServer creates a new health check server.
func NewServer(port string, component string) *Server {
	s := &Server{
		port:      port,
		component: component,
		checks: map[string]CheckStatus{
			"config": CheckError,
			"broker": CheckError,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthzHandler)
	mux.HandleFunc("/readyz", s.readyzHandler)
	mux.HandleFunc("/config", s.configHandler)

	s.server = &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	return s
}

// Start starts the health server in a goroutine.
func (s *Server) Start(ctx context.Context) error {
	slog.InfoContext(ctx, "starting health server", "port", s.port)

	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.ErrorContext(ctx, "health server error", "error", err)
		}
	}()

	return nil
}

// Shutdown gracefully shuts down the health server.
func (s *Server) Shutdown(ctx context.Context) error {
	slog.InfoContext(ctx, "shutting down health server...")
	return s.server.Shutdown(ctx)
}

// SetCheck sets the status of a specific health check.
func (s *Server) SetCheck(name string, status CheckStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks[name] = status
}

// SetBrokerReady sets the broker check status.
func (s *Server) SetBrokerReady(ready bool) {
	if ready {
		s.SetCheck("broker", CheckOK)
	} else {
		s.SetCheck("broker", CheckError)
	}
}

// SetConfigLoaded marks the config check as ok.
func (s *Server) SetConfigLoaded() {
	s.SetCheck("config", CheckOK)
}

// SetConfig stores pre-marshaled YAML config to serve at /config.
// Only call this when debug_config is enabled — the endpoint returns 404 otherwise.
func (s *Server) SetConfig(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configYAML = data
}

// SetShuttingDown marks the server as shutting down.
// When set to true, /readyz will immediately return 503 Service Unavailable
// regardless of other check statuses. This follows the HyperFleet Graceful
// Shutdown Standard: mark not ready immediately when SIGTERM is received.
func (s *Server) SetShuttingDown(shuttingDown bool) {
	s.shuttingDown.Store(shuttingDown)
}

// IsShuttingDown returns true if the server is in shutdown mode.
func (s *Server) IsShuttingDown() bool {
	return s.shuttingDown.Load()
}

// IsReady returns true if all checks are passing and server is not shutting down.
func (s *Server) IsReady() bool {
	// Check shutdown flag first (atomic, no lock needed)
	if s.shuttingDown.Load() {
		return false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, status := range s.checks {
		if status != CheckOK {
			return false
		}
	}
	return true
}

// healthzHandler handles liveness probe requests.
// Returns 200 OK if the process is alive.
func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(HealthResponse{Status: "ok"}) //nolint:errcheck // best-effort response
}

// readyzHandler handles readiness probe requests.
// Returns 200 OK with detailed checks if all checks pass,
// 503 Service Unavailable if shutting down or any check fails.
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Check shutdown flag first (atomic, no lock needed)
	// Per HyperFleet Graceful Shutdown Standard: immediately return 503 on shutdown
	if s.shuttingDown.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		//nolint:errcheck // best-effort response
		_ = json.NewEncoder(w).Encode(ReadyResponse{
			Status:  "error",
			Message: "server is shutting down",
		})
		return
	}

	s.mu.RLock()
	checks := make(map[string]CheckStatus, len(s.checks))
	allOK := true
	for name, status := range s.checks {
		checks[name] = status
		if status != CheckOK {
			allOK = false
		}
	}
	s.mu.RUnlock()

	if allOK {
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // best-effort response
		_ = json.NewEncoder(w).Encode(ReadyResponse{
			Status: "ok",
			Checks: checks,
		})
		return
	}

	w.WriteHeader(http.StatusServiceUnavailable)
	//nolint:errcheck // best-effort response
	_ = json.NewEncoder(w).Encode(ReadyResponse{
		Status:  "error",
		Message: "not ready",
		Checks:  checks,
	})
}

// configHandler serves the current adapter configuration as YAML.
// Returns 404 if debug_config is not enabled (SetConfig was never called).
func (s *Server) configHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	data := s.configYAML
	s.mu.RUnlock()

	if data == nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data) //nolint:errcheck // best-effort response
}
