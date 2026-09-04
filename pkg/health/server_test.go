package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthzHandler(t *testing.T) {
	server := NewServer("8080", "test-adapter")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	server.healthzHandler(w, req)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var response HealthResponse
	err := json.NewDecoder(resp.Body).Decode(&response)
	require.NoError(t, err)
	assert.Equal(t, "ok", response.Status)
	assert.Empty(t, response.Message)
}

func TestReadyzHandler_NotReady(t *testing.T) {
	server := NewServer("8080", "test-adapter")
	// By default, checks are in error state

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	server.readyzHandler(w, req)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var response ReadyResponse
	err := json.NewDecoder(resp.Body).Decode(&response)
	require.NoError(t, err)
	assert.Equal(t, "error", response.Status)
	assert.Equal(t, "not ready", response.Message)
	assert.NotNil(t, response.Checks)
	assert.Equal(t, CheckError, response.Checks["config"])
	assert.Equal(t, CheckError, response.Checks["broker"])
}

func TestReadyzHandler_Ready(t *testing.T) {
	server := NewServer("8080", "test-adapter")
	server.SetConfigLoaded()
	server.SetBrokerReady(true)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	server.readyzHandler(w, req)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var response ReadyResponse
	err := json.NewDecoder(resp.Body).Decode(&response)
	require.NoError(t, err)
	assert.Equal(t, "ok", response.Status)
	assert.Empty(t, response.Message)
	assert.NotNil(t, response.Checks)
	assert.Equal(t, CheckOK, response.Checks["config"])
	assert.Equal(t, CheckOK, response.Checks["broker"])
}

func TestSetBrokerReady(t *testing.T) {
	server := NewServer("8080", "test-adapter")

	// Initially not ready (both checks are error)
	assert.False(t, server.IsReady())

	// Set config loaded
	server.SetConfigLoaded()
	assert.False(t, server.IsReady()) // Still not ready, broker is error

	// Set broker ready
	server.SetBrokerReady(true)
	assert.True(t, server.IsReady()) // Now ready

	// Set broker not ready again
	server.SetBrokerReady(false)
	assert.False(t, server.IsReady())
}

func TestSetCheck(t *testing.T) {
	server := NewServer("8080", "test-adapter")

	// Set a custom check
	server.SetCheck("custom", CheckOK)
	server.SetConfigLoaded()
	server.SetBrokerReady(true)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	server.readyzHandler(w, req)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	var response ReadyResponse
	err := json.NewDecoder(resp.Body).Decode(&response)
	require.NoError(t, err)
	assert.Equal(t, "ok", response.Status)
	assert.Equal(t, CheckOK, response.Checks["custom"])
	assert.Equal(t, CheckOK, response.Checks["config"])
	assert.Equal(t, CheckOK, response.Checks["broker"])
}

func TestReadyzHandler_PartialReady(t *testing.T) {
	server := NewServer("8080", "test-adapter")

	// Only config is loaded, broker is not ready
	server.SetConfigLoaded()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	server.readyzHandler(w, req)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	var response ReadyResponse
	err := json.NewDecoder(resp.Body).Decode(&response)
	require.NoError(t, err)
	assert.Equal(t, "error", response.Status)
	assert.Equal(t, CheckOK, response.Checks["config"])
	assert.Equal(t, CheckError, response.Checks["broker"])
}

func TestReadyzHandler_ReadyToNotReady(t *testing.T) {
	server := NewServer("8080", "test-adapter")

	// Set all checks to ok
	server.SetConfigLoaded()
	server.SetBrokerReady(true)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	server.readyzHandler(w, req)
	assert.Equal(t, http.StatusOK, w.Result().StatusCode)

	// Set not ready (simulating shutdown)
	server.SetBrokerReady(false)

	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w = httptest.NewRecorder()
	server.readyzHandler(w, req)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	var response ReadyResponse
	err := json.NewDecoder(resp.Body).Decode(&response)
	require.NoError(t, err)
	assert.Equal(t, CheckOK, response.Checks["config"])    // Config stays ok
	assert.Equal(t, CheckError, response.Checks["broker"]) // Broker is error
}

func TestSetShuttingDown(t *testing.T) {
	server := NewServer("8080", "test-adapter")

	// Initially not shutting down
	assert.False(t, server.IsShuttingDown())

	// Set shutting down
	server.SetShuttingDown(true)
	assert.True(t, server.IsShuttingDown())

	// Clear shutting down
	server.SetShuttingDown(false)
	assert.False(t, server.IsShuttingDown())
}

func TestReadyzHandler_ShuttingDown(t *testing.T) {
	server := NewServer("8080", "test-adapter")

	// Set all checks to ok (server is ready)
	server.SetConfigLoaded()
	server.SetBrokerReady(true)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	server.readyzHandler(w, req)
	assert.Equal(t, http.StatusOK, w.Result().StatusCode)

	// Simulate graceful shutdown - set shutting down flag
	server.SetShuttingDown(true)

	// Readyz should now return 503 with shutdown message
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w = httptest.NewRecorder()
	server.readyzHandler(w, req)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	var response ReadyResponse
	err := json.NewDecoder(resp.Body).Decode(&response)
	require.NoError(t, err)
	assert.Equal(t, "error", response.Status)
	assert.Equal(t, "server is shutting down", response.Message)
	// Checks should not be included when shutting down
	assert.Nil(t, response.Checks)
}

func TestIsReady_ShuttingDown(t *testing.T) {
	server := NewServer("8080", "test-adapter")

	// Set all checks to ok
	server.SetConfigLoaded()
	server.SetBrokerReady(true)
	assert.True(t, server.IsReady())

	// Set shutting down - should override checks
	server.SetShuttingDown(true)
	assert.False(t, server.IsReady())
}

func TestReadyzHandler_ShuttingDownPriority(t *testing.T) {
	server := NewServer("8080", "test-adapter")

	// Set all checks to ok
	server.SetConfigLoaded()
	server.SetBrokerReady(true)

	// Set shutting down first, then check response
	server.SetShuttingDown(true)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	server.readyzHandler(w, req)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	// Should return shutdown message, not regular "not ready" message
	var response ReadyResponse
	err := json.NewDecoder(resp.Body).Decode(&response)
	require.NoError(t, err)
	assert.Equal(t, "server is shutting down", response.Message)
}

func TestServerLifecycle_StartAndShutdown(t *testing.T) {
	// Use a unique port to avoid conflicts
	port := "18080"
	server := NewServer(port, "test-adapter")

	ctx := context.Background()

	// Start the server
	err := server.Start(ctx)
	require.NoError(t, err)

	// Give the server time to start listening
	time.Sleep(50 * time.Millisecond)

	// Verify server is listening by making an HTTP request to /healthz
	resp, err := http.Get("http://localhost:" + port + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var healthResp HealthResponse
	err = json.NewDecoder(resp.Body).Decode(&healthResp)
	require.NoError(t, err)
	assert.Equal(t, "ok", healthResp.Status)

	// Shutdown the server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = server.Shutdown(shutdownCtx)
	require.NoError(t, err)

	// Give the server time to fully shutdown
	time.Sleep(50 * time.Millisecond)

	// Verify server stopped accepting connections
	_, err = http.Get("http://localhost:" + port + "/healthz")
	assert.Error(t, err, "expected connection refused after shutdown")
}

func TestServerLifecycle_ReadyzWhileRunning(t *testing.T) {
	port := "18081"
	server := NewServer(port, "test-adapter")

	ctx := context.Background()

	// Start the server
	err := server.Start(ctx)
	require.NoError(t, err)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	// Give the server time to start listening
	time.Sleep(50 * time.Millisecond)

	// Initially not ready (checks are in error state)
	assert.False(t, server.IsReady())

	resp, err := http.Get("http://localhost:" + port + "/readyz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	// Set checks to ok
	server.SetConfigLoaded()
	server.SetBrokerReady(true)
	assert.True(t, server.IsReady())

	resp2, err := http.Get("http://localhost:" + port + "/readyz")
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
}

func TestServerLifecycle_GracefulShutdownStateTransitions(t *testing.T) {
	port := "18082"
	server := NewServer(port, "test-adapter")

	ctx := context.Background()

	// Start the server
	err := server.Start(ctx)
	require.NoError(t, err)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	// Give the server time to start listening
	time.Sleep(50 * time.Millisecond)

	// Set server to ready state
	server.SetConfigLoaded()
	server.SetBrokerReady(true)

	// Verify initial state
	assert.True(t, server.IsReady())
	assert.False(t, server.IsShuttingDown())

	// Verify /readyz returns 200
	resp, err := http.Get("http://localhost:" + port + "/readyz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Simulate graceful shutdown by setting shuttingDown flag
	server.SetShuttingDown(true)

	// Verify state transitions
	assert.True(t, server.IsShuttingDown())
	assert.False(t, server.IsReady()) // IsReady should return false when shutting down

	// Verify /readyz now returns 503 with shutdown message
	resp2, err := http.Get("http://localhost:" + port + "/readyz")
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusServiceUnavailable, resp2.StatusCode)

	var readyResp ReadyResponse
	err = json.NewDecoder(resp2.Body).Decode(&readyResp)
	require.NoError(t, err)
	assert.Equal(t, "error", readyResp.Status)
	assert.Equal(t, "server is shutting down", readyResp.Message)

	// Verify /healthz still returns 200 (liveness should work during shutdown)
	resp3, err := http.Get("http://localhost:" + port + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp3.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
}

func TestServerLifecycle_ShutdownTimeout(t *testing.T) {
	port := "18083"
	server := NewServer(port, "test-adapter")

	ctx := context.Background()

	// Start the server
	err := server.Start(ctx)
	require.NoError(t, err)

	// Give the server time to start listening
	time.Sleep(50 * time.Millisecond)

	// Shutdown with a very short timeout (should still succeed for idle server)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err = server.Shutdown(shutdownCtx)
	require.NoError(t, err)
}
