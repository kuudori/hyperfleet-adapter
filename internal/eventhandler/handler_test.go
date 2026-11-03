package eventhandler

import (
	"context"
	"fmt"
	"testing"

	"github.com/cloudevents/sdk-go/v2/event"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient/desiretest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/executor"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportregistry"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/metrics"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
)

// TestCreateHandler_MetricsRecording verifies that WithMetrics records Prometheus metrics
func TestCreateHandler_MetricsRecording(t *testing.T) {
	tests := []struct {
		name           string
		expectedStatus string // "success", "skipped", or "failed"
		preconditions  []configloader.Precondition
	}{
		{
			name:           "success records success metric",
			preconditions:  []configloader.Precondition{},
			expectedStatus: "success",
		},
		{
			name: "skipped records skipped metric",
			preconditions: []configloader.Precondition{
				{ActionBase: configloader.ActionBase{Name: "check"}, Expression: "false"},
			},
			expectedStatus: "skipped",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder, registry := newTestRecorder(t)

			config := &configloader.Config{
				Adapter:       configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
				Preconditions: tt.preconditions,
			}

			exec := newTestExecutor(t, config, hyperfleetapi.NewMockClient())

			handler := AlwaysAck(WithMetrics(CreateHandler(exec), recorder))

			evt := newTestEvent(t, "test-event-1")
			require.NoError(t, evt.SetData(event.ApplicationJSON, []byte(`{"id":"cluster-1"}`)))

			err := handler(context.Background(), evt)
			require.NoError(t, err, "handler should always return nil")

			// Verify events_processed_total
			families, err := registry.Gather()
			require.NoError(t, err)

			eventsCount := getCounterValue(t, families, "hyperfleet_adapter_events_processed_total", "status", tt.expectedStatus)
			assert.Equal(t, float64(1), eventsCount, "expected 1 event with status %s", tt.expectedStatus)

			// Verify duration was recorded
			durationFamily := findFamily(families, "hyperfleet_adapter_event_processing_duration_seconds")
			require.NotNil(t, durationFamily, "duration metric should exist")
			histogram := durationFamily.GetMetric()[0].GetHistogram()
			assert.Equal(t, uint64(1), histogram.GetSampleCount(), "expected 1 duration sample")
		})
	}
}

// TestCreateHandler_MetricsRecording_Failed verifies error metrics are recorded on failure
func TestCreateHandler_MetricsRecording_Failed(t *testing.T) {
	recorder, registry := newTestRecorder(t)

	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
		Params: []configloader.Parameter{
			{Name: "required", Source: configloader.StringSource("env.MISSING_VAR"), Required: true},
		},
	}

	exec := newTestExecutor(t, config, hyperfleetapi.NewMockClient())

	handler := AlwaysAck(WithMetrics(CreateHandler(exec), recorder))

	evt := newTestEvent(t, "test-event-fail")
	require.NoError(t, evt.SetData(event.ApplicationJSON, []byte(`{"id":"cluster-1"}`)))

	err := handler(context.Background(), evt)
	require.NoError(t, err, "handler should always return nil even on failure")

	families, err := registry.Gather()
	require.NoError(t, err)

	// Verify failed event was recorded
	failedCount := getCounterValue(t, families, "hyperfleet_adapter_events_processed_total", "status", "failed")
	assert.Equal(t, float64(1), failedCount, "expected 1 failed event")

	// Verify error was recorded with phase label
	errorCount := getCounterValue(t, families, "hyperfleet_adapter_errors_total", "error_type", "param_extraction")
	assert.Equal(t, float64(1), errorCount, "expected 1 param_extraction error")
}

// TestCreateHandler_NilMetricsRecorder verifies handler works without a metrics recorder
func TestCreateHandler_NilMetricsRecorder(t *testing.T) {
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
	}

	exec := newTestExecutor(t, config, hyperfleetapi.NewMockClient())

	handler := AlwaysAck(WithMetrics(CreateHandler(exec), nil))

	evt := newTestEvent(t, "test-event-nil")
	require.NoError(t, evt.SetData(event.ApplicationJSON, []byte(`{"id":"cluster-1"}`)))

	assert.NotPanics(t, func() {
		_ = handler(context.Background(), evt)
	}, "handler with nil MetricsRecorder should not panic")
}

func TestCreateHandler_ContinuesUpstreamTrace(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		// Shutdown only flushes to the in-memory SpanRecorder, which cannot fail.
		_ = provider.Shutdown(context.Background())
	})

	exec := newTestExecutor(t, &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
	}, hyperfleetapi.NewMockClient())

	evt := newTestEvent(t, "test-event-trace")
	evt.SetExtension("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	require.NoError(t, evt.SetData(event.ApplicationJSON, []byte(`{"id":"cluster-1"}`)))

	_, err := CreateHandler(exec)(t.Context(), evt)
	require.NoError(t, err)

	ended := spans.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, "Execute", ended[0].Name())
	assert.Equal(t, "0af7651916cd43dd8448eb211c80319c", ended[0].SpanContext().TraceID().String())
	assert.Equal(t, "b7ad6b7169203331", ended[0].Parent().SpanID().String())
}

// TestCreateHandler_PendingDeleteDoesNotRecordErrorMetric verifies that an apply refused
// because a delete is still pending counts as a success, neither failed nor skipped
func TestCreateHandler_PendingDeleteDoesNotRecordErrorMetric(t *testing.T) {
	const (
		transportName = "desire-primary"
		owner         = "hyperfleet-adapter"
	)
	identity := desiretest.TestIdentity{
		ManagementCluster: "cluster-1",
		Resource:          "configmaps",
		Namespace:         "default",
		Name:              "test-config",
	}
	definition := configloader.TransportDefinition{
		Type:            configloader.TransportTypeRemote,
		TargetCluster:   identity.ManagementCluster,
		ResourcePlurals: map[string]string{"v1/ConfigMap": identity.Resource},
	}

	store := memory.New()
	_, err := store.CreateDeleteDesire(t.Context(), desire.DeleteDesire{
		Identity: identity.Delete(),
		Owner:    owner,
	})
	require.NoError(t, err)

	exec, err := executor.NewBuilder().
		WithConfig(&configloader.Config{
			Adapter:    configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
			Transports: map[string]configloader.TransportDefinition{transportName: definition},
			Resources: []configloader.Resource{{
				Name:      "test-resource",
				Transport: configloader.NamedTransport(transportName),
				Manifest: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name":        identity.Name,
						"namespace":   identity.Namespace,
						"annotations": map[string]interface{}{"hyperfleet.io/generation": "1"},
					},
					"data": map[string]interface{}{"key": "value"},
				},
				Discovery: &configloader.DiscoveryConfig{Namespace: identity.Namespace, ByName: identity.Name},
				Lifecycle: &configloader.ResourceLifecycle{
					Delete: &configloader.LifecycleDelete{PropagationPolicy: "Background"},
				},
			}},
		}).
		WithAPIClient(hyperfleetapi.NewMockClient()).
		WithTransportRegistry(transportclient.Registry{
			transportName: {
				Client: desireclient.NewClient(store, owner),
				Target: transportregistry.Target(transportName, definition),
			},
		}).
		Build()
	require.NoError(t, err)

	recorder, registry := newTestRecorder(t)
	result, err := WithMetrics(CreateHandler(exec), recorder)(t.Context(), newTestEvent(t, "pending-delete"))
	require.NoError(t, err)
	require.Equal(t, executor.StatusSuccess, result.Status, "errors=%v", result.Errors)
	require.Len(t, result.ResourceResults, 1)
	require.Equal(t, "delete pending, retrying on next event", result.ResourceResults[0].OperationReason)

	families, err := registry.Gather()
	require.NoError(t, err)
	assert.Equal(t, float64(1), getCounterValue(t, families,
		"hyperfleet_adapter_events_processed_total", "status", "success"))
	assert.Zero(t, getCounterValue(t, families,
		"hyperfleet_adapter_events_processed_total", "status", "failed"))
	assert.Nil(t, findFamily(families, "hyperfleet_adapter_errors_total"))
}

// TestWithMetrics_RecordsMetrics verifies WithMetrics records the correct metric status
// and passes the result through
func TestWithMetrics_RecordsMetrics(t *testing.T) {
	tests := []struct {
		result          *executor.ExecutionResult
		err             error
		name            string
		expectedStatus  string
		expectNoMetrics bool
	}{
		{
			name:           "success",
			result:         &executor.ExecutionResult{Status: executor.StatusSuccess},
			expectedStatus: "success",
		},
		{
			name:           "skipped",
			result:         &executor.ExecutionResult{Status: executor.StatusSuccess, ResourcesSkipped: true},
			expectedStatus: "skipped",
		},
		{
			name: "failed",
			result: &executor.ExecutionResult{
				Status: executor.StatusFailed,
				Errors: map[executor.ExecutionPhase]error{executor.PhaseParamExtraction: fmt.Errorf("error")},
			},
			expectedStatus: "failed",
		},
		{
			name:           "inner handler error with nil result",
			err:            fmt.Errorf("inner handler error"),
			expectedStatus: "failed",
		},
		{
			name:           "inner handler error with success result",
			result:         &executor.ExecutionResult{Status: executor.StatusSuccess},
			err:            fmt.Errorf("inner handler error"),
			expectedStatus: "failed",
		},
		{
			name:            "nil result",
			result:          nil,
			expectNoMetrics: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder, registry := newTestRecorder(t)

			inner := HandlerFunc(func(_ context.Context, _ *event.Event) (*executor.ExecutionResult, error) {
				return tt.result, tt.err
			})
			handler := WithMetrics(inner, recorder)

			evt := newTestEvent(t, "test-metrics-"+tt.name)

			got, err := handler(context.Background(), evt)
			require.ErrorIs(t, err, tt.err)
			assert.Equal(t, tt.result, got, "result must be passed through unchanged")

			families, err := registry.Gather()
			require.NoError(t, err)

			if tt.expectNoMetrics {
				assert.Nil(t, findFamily(families, "hyperfleet_adapter_events_processed_total"),
					"no status counter should be recorded for nil result")
				durationFamily := findFamily(families, "hyperfleet_adapter_event_processing_duration_seconds")
				require.NotNil(t, durationFamily, "duration must be recorded even for nil result")
				assert.Equal(t, uint64(1), durationFamily.GetMetric()[0].GetHistogram().GetSampleCount())
				return
			}

			count := getCounterValue(t, families, "hyperfleet_adapter_events_processed_total", "status", tt.expectedStatus)
			assert.Equal(t, float64(1), count)
			if tt.expectedStatus != "failed" {
				assert.Zero(t, getCounterValue(t, families,
					"hyperfleet_adapter_events_processed_total", "status", "failed"))
				assert.Nil(t, findFamily(families, "hyperfleet_adapter_errors_total"))
			}

			durationFamily := findFamily(families, "hyperfleet_adapter_event_processing_duration_seconds")
			require.NotNil(t, durationFamily)
			assert.Equal(t, uint64(1), durationFamily.GetMetric()[0].GetHistogram().GetSampleCount())
		})
	}
}

// TestWithMetrics_HandlerPanicPropagates verifies a panic in handler is not swallowed by WithMetrics
func TestWithMetrics_HandlerPanicPropagates(t *testing.T) {
	inner := HandlerFunc(func(_ context.Context, _ *event.Event) (*executor.ExecutionResult, error) {
		panic("handler panic")
	})

	recorder, _ := newTestRecorder(t)
	handler := WithMetrics(inner, recorder)

	evt := newTestEvent(t, "test-handler-panic")

	assert.Panics(t, func() {
		_, _ = handler(context.Background(), evt)
	}, "panic in inner handler must propagate through WithMetrics")
}

// TestWithMetrics_MetricsPanicRecovered verifies that a panic inside metrics recording
// is recovered
func TestWithMetrics_MetricsPanicRecovered(t *testing.T) {
	inner := HandlerFunc(func(_ context.Context, _ *event.Event) (*executor.ExecutionResult, error) {
		return &executor.ExecutionResult{Status: executor.StatusSuccess}, nil
	})

	// new(metrics.Recorder) bypasses the nil receiver guard but has nil internal
	// fields, causing panic inside recordMetrics
	panicRecorder := new(metrics.Recorder)
	handler := WithMetrics(inner, panicRecorder)

	evt := newTestEvent(t, "test-metrics-panic")

	var got *executor.ExecutionResult
	assert.NotPanics(t, func() {
		got, _ = handler(context.Background(), evt)
	}, "panic in metrics recording must be recovered by WithMetrics")
	assert.NotNil(t, got, "result must still be returned after metrics panic")
}

// TestAlwaysAck_AlwaysReturnsNil verifies AlwaysAck always returns nil
func TestAlwaysAck_AlwaysReturnsNil(t *testing.T) {
	tests := []struct {
		result *executor.ExecutionResult
		err    error
		name   string
	}{
		{
			name:   "success result",
			result: &executor.ExecutionResult{Status: executor.StatusSuccess},
		},
		{
			name:   "failed result",
			result: &executor.ExecutionResult{Status: executor.StatusFailed},
		},
		{
			name: "error from inner handler",
			err:  fmt.Errorf("inner handler error"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := HandlerFunc(func(_ context.Context, _ *event.Event) (*executor.ExecutionResult, error) {
				return tt.result, tt.err
			})

			handler := AlwaysAck(inner)

			evt := newTestEvent(t, "test-ack")

			err := handler(context.Background(), evt)
			assert.Nil(t, err, "AlwaysAck must always return nil")
		})
	}
}

// helper functions for metrics assertions

func findFamily(families []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

func getCounterValue(t *testing.T, families []*dto.MetricFamily, metricName, labelName, labelValue string) float64 {
	t.Helper()
	family := findFamily(families, metricName)
	if family == nil {
		t.Fatalf("metric %s not found", metricName)
	}
	for _, m := range family.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == labelName && l.GetValue() == labelValue {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}
