package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/cloudevents/sdk-go/v2/event"
	hyperfleetlogger "github.com/openshift-hyperfleet/hyperfleet-logger"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/k8sclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/metrics"
)

func TestCreateHandler_PostActionAPIAuthFailuresAreAcknowledgedAndRecorded(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{name: "401 unauthorized", statusCode: http.StatusUnauthorized},
		{name: "403 forbidden", statusCode: http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			recorder := metrics.NewRecorder("test-adapter", "v0.1.0", "test", registry)
			mockClient := newMockAPIClient()
			mockClient.PutResponse = &hyperfleetapi.Response{
				StatusCode: tt.statusCode,
				Status:     fmt.Sprintf("%d %s", tt.statusCode, http.StatusText(tt.statusCode)),
				Body:       []byte(`{"error":"authentication failed"}`),
				Attempts:   1,
			}

			exec, err := NewBuilder().
				WithConfig(authFailurePostActionConfig()).
				WithAPIClient(mockClient).
				WithTransportClient(k8sclient.NewMockK8sClient()).
				Build()
			require.NoError(t, err)

			handler := AlwaysAck(WithMetrics(exec.CreateHandler(), recorder))
			err = handler(context.Background(), authFailureEvent(t, "cluster-auth-failure"))
			require.NoError(t, err, "authentication failures must be ACKed for external remediation")

			families, err := registry.Gather()
			require.NoError(t, err)
			assert.Equal(t, float64(1), getCounterValue(t, families,
				"hyperfleet_adapter_events_processed_total", "status", "failed"),
				"auth-failed post-action must fail the event")
			assert.Equal(t, float64(1), getCounterValue(t, families,
				"hyperfleet_adapter_errors_total", "error_type", "post_actions"),
				"post_actions phase error metric behavior must remain unchanged")
			assert.Equal(t, float64(1), getCounterValue(t, families,
				"hyperfleet_adapter_api_auth_failures_total", "status_code", fmt.Sprint(tt.statusCode)),
				"auth failures must be classified by exact bounded HTTP status")
		})
	}
}

func TestCreateHandler_PostActionNonAuthAPIFailureDoesNotEmitAuthMetric(t *testing.T) {
	registry := prometheus.NewRegistry()
	recorder := metrics.NewRecorder("test-adapter", "v0.1.0", "test", registry)
	mockClient := newMockAPIClient()
	mockClient.PutResponse = &hyperfleetapi.Response{
		StatusCode: http.StatusInternalServerError,
		Status:     "500 Internal Server Error",
		Body:       []byte(`{"error":"upstream unavailable"}`),
		Attempts:   1,
	}

	exec, err := NewBuilder().
		WithConfig(authFailurePostActionConfig()).
		WithAPIClient(mockClient).
		WithTransportClient(k8sclient.NewMockK8sClient()).
		Build()
	require.NoError(t, err)

	handler := AlwaysAck(WithMetrics(exec.CreateHandler(), recorder))
	err = handler(context.Background(), authFailureEvent(t, "cluster-non-auth-failure"))
	require.NoError(t, err, "existing always-ACK behavior for non-auth failures must remain unchanged")

	families, err := registry.Gather()
	require.NoError(t, err)
	assert.Equal(t, float64(1), getCounterValue(t, families,
		"hyperfleet_adapter_events_processed_total", "status", "failed"))
	assert.Equal(t, float64(1), getCounterValue(t, families,
		"hyperfleet_adapter_errors_total", "error_type", "post_actions"))
	assert.Nil(t, findFamily(families, "hyperfleet_adapter_api_auth_failures_total"),
		"non-auth API failures must not create an auth-failure metric series")
}

func TestCreateHandler_OptionalAPIParameterAuthFailureIsLoggedAndRecorded(t *testing.T) {
	previous := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(hyperfleetlogger.NewHandler(
		"test",
		"test",
		hyperfleetlogger.WithLevel(slog.LevelDebug),
		hyperfleetlogger.WithFormat(hyperfleetlogger.FormatText),
		hyperfleetlogger.WithOutput(&logs),
	)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	registry := prometheus.NewRegistry()
	recorder := metrics.NewRecorder("test-adapter", "v0.1.0", "test", registry)
	mockClient := newMockAPIClient()
	mockClient.GetResponse = &hyperfleetapi.Response{
		StatusCode: http.StatusForbidden,
		Status:     "403 Forbidden",
		Body:       []byte(`optional-api-parameter-auth-response-must-not-be-logged`),
	}

	exec, err := NewBuilder().
		WithConfig(&configloader.Config{
			Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
			Params: []configloader.Parameter{
				{Name: "clusterID", Source: configloader.StringSource("event.id"), Required: true},
				{
					Name: "cluster",
					Source: configloader.APICallSource(&configloader.APICall{
						Method: http.MethodGet,
						URL:    "/clusters/{{ .clusterID }}",
					}),
					Default: map[string]interface{}{"name": "fallback"},
				},
			},
		}).
		WithAPIClient(mockClient).
		WithTransportClient(k8sclient.NewMockK8sClient()).
		Build()
	require.NoError(t, err)

	result, err := WithMetrics(exec.CreateHandler(), recorder)(
		context.Background(), authFailureEvent(t, "cluster-optional-auth-failure"))
	require.NoError(t, err)
	require.Equal(t, StatusSuccess, result.Status)
	assert.Equal(t, map[string]interface{}{"name": "fallback"}, result.Params["cluster"])

	families, err := registry.Gather()
	require.NoError(t, err)
	assert.Equal(t, float64(1), getCounterValue(t, families,
		"hyperfleet_adapter_events_processed_total", "status", "success"))
	assert.Equal(t, float64(1), getCounterValue(t, families,
		"hyperfleet_adapter_api_auth_failures_total", "status_code", "403"))
	assert.Contains(t, logs.String(), "http_status=403")
	assert.Contains(t, logs.String(), "phase=param_extraction")
	assert.Contains(t, logs.String(), "param=cluster")
	assert.NotContains(t, logs.String(), "optional-api-parameter-auth-response-must-not-be-logged")
}

func TestExecutor_PostActionAuthFailureLogsAreContextualAndRedacted(t *testing.T) {
	tests := []struct {
		sentinel      string
		name          string
		statusCode    int
		expectAuthLog bool
	}{
		{
			name:          "401 unauthorized",
			statusCode:    http.StatusUnauthorized,
			sentinel:      "qe-auth-response-body-401-must-not-be-logged",
			expectAuthLog: true,
		},
		{
			name:          "403 forbidden",
			statusCode:    http.StatusForbidden,
			sentinel:      "qe-auth-response-body-403-must-not-be-logged",
			expectAuthLog: true,
		},
		{
			name:       "500 server error",
			statusCode: http.StatusInternalServerError,
			sentinel:   "qe-non-auth-response-body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previous := slog.Default()
			var logs bytes.Buffer
			slog.SetDefault(slog.New(hyperfleetlogger.NewHandler(
				"test",
				"test",
				hyperfleetlogger.WithLevel(slog.LevelDebug),
				hyperfleetlogger.WithFormat(hyperfleetlogger.FormatText),
				hyperfleetlogger.WithOutput(&logs),
			)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			mockClient := newMockAPIClient()
			mockClient.GetResponse = &hyperfleetapi.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       []byte(`{}`),
			}
			mockClient.PutResponse = &hyperfleetapi.Response{
				StatusCode: tt.statusCode,
				Status:     fmt.Sprintf("%d %s", tt.statusCode, http.StatusText(tt.statusCode)),
				Body:       []byte(tt.sentinel),
			}

			exec, err := NewBuilder().
				WithConfig(new404PostActionConfig()).
				WithAPIClient(mockClient).
				WithTransportClient(k8sclient.NewMockK8sClient()).
				Build()
			require.NoError(t, err)

			result := exec.Execute(context.Background(), map[string]interface{}{
				"id":   "cluster-auth-401",
				"kind": "Cluster",
			})
			require.Equal(t, StatusFailed, result.Status, "post-action API failure must fail the execution")

			captured := logs.String()
			assert.NotContains(t, captured, tt.sentinel, "response bodies must never be written to executor logs")
			if !tt.expectAuthLog {
				assert.NotContains(t, captured, "http_status=500",
					"non-auth API failures must not emit the dedicated auth-failure log")
				return
			}

			assert.Contains(t, captured, "ERROR")
			assert.Contains(t, captured, fmt.Sprintf("http_status=%d", tt.statusCode))
			assert.Contains(t, captured, "post_action=reportStatus")
			assert.Contains(t, captured, "cluster_id=cluster-auth-401")
		})
	}
}

func TestExecutor_APIAuthFailuresAreLoggedAcrossExecutionPhases(t *testing.T) {
	tests := []struct {
		name         string
		phase        ExecutionPhase
		config       *configloader.Config
		responseBody string
	}{
		{
			name:         "parameter extraction",
			phase:        PhaseParamExtraction,
			responseBody: "parameter-auth-response-must-not-be-logged",
			config: &configloader.Config{
				Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
				Params: []configloader.Parameter{
					{Name: "clusterID", Source: configloader.StringSource("event.id"), Required: true},
					{
						Name:     "cluster",
						Required: true,
						Source: configloader.APICallSource(&configloader.APICall{
							Method: http.MethodGet,
							URL:    "/clusters/{{ .clusterID }}",
						}),
					},
				},
			},
		},
		{
			name:         "preconditions",
			phase:        PhasePreconditions,
			responseBody: "precondition-auth-response-must-not-be-logged",
			config: &configloader.Config{
				Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
				Params: []configloader.Parameter{
					{Name: "clusterID", Source: configloader.StringSource("event.id"), Required: true},
				},
				Preconditions: []configloader.Precondition{
					{
						ActionBase: configloader.ActionBase{
							Name: "fetch-cluster",
							APICall: &configloader.APICall{
								Method: http.MethodGet,
								URL:    "/clusters/{{ .clusterID }}",
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previous := slog.Default()
			var logs bytes.Buffer
			slog.SetDefault(slog.New(hyperfleetlogger.NewHandler(
				"test",
				"test",
				hyperfleetlogger.WithLevel(slog.LevelDebug),
				hyperfleetlogger.WithFormat(hyperfleetlogger.FormatText),
				hyperfleetlogger.WithOutput(&logs),
			)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			registry := prometheus.NewRegistry()
			recorder := metrics.NewRecorder("test-adapter", "v0.1.0", "test", registry)
			mockClient := newMockAPIClient()
			mockClient.GetResponse = &hyperfleetapi.Response{
				StatusCode: http.StatusForbidden,
				Status:     "403 Forbidden",
				Body:       []byte(tt.responseBody),
			}

			exec, err := NewBuilder().
				WithConfig(tt.config).
				WithAPIClient(mockClient).
				WithTransportClient(k8sclient.NewMockK8sClient()).
				Build()
			require.NoError(t, err)

			result, err := WithMetrics(exec.CreateHandler(), recorder)(
				context.Background(), authFailureEvent(t, "cluster-auth-failure"))
			require.NoError(t, err)
			require.Equal(t, StatusFailed, result.Status)

			assert.Contains(t, logs.String(), "http_status=403")
			assert.Contains(t, logs.String(), "phase="+string(tt.phase))
			assert.NotContains(t, logs.String(), tt.responseBody)

			families, err := registry.Gather()
			require.NoError(t, err)
			assert.Equal(t, float64(1), getCounterValue(t, families,
				"hyperfleet_adapter_api_auth_failures_total", "status_code", "403"))
		})
	}
}

func authFailurePostActionConfig() *configloader.Config {
	return &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
		Post: &configloader.PostConfig{PostActions: []configloader.PostAction{
			{
				ActionBase: configloader.ActionBase{
					Name: "write-cluster-status",
					APICall: &configloader.APICall{
						Method: http.MethodPut,
						URL:    "/clusters/{{ .clusterID }}/status",
						Body:   `{"status":"ready"}`,
					},
				},
			},
		}},
		Params: []configloader.Parameter{
			{Name: "clusterID", Source: configloader.StringSource("event.id"), Required: true},
		},
	}
}

func authFailureEvent(t *testing.T, clusterID string) *event.Event {
	t.Helper()
	evt := event.New()
	evt.SetID("event-" + clusterID)
	evt.SetType("com.hyperfleet.test")
	evt.SetSource("qe")
	payload, err := json.Marshal(map[string]interface{}{"id": clusterID})
	require.NoError(t, err)
	require.NoError(t, evt.SetData(event.ApplicationJSON, payload))
	return &evt
}
