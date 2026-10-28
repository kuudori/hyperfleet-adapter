package eventhandler

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cloudevents/sdk-go/v2/event"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/executor"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/k8sclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/metrics"
)

// TestCreateHandler_MetricsRecording verifies that WithMetrics records Prometheus metrics
func TestCreateHandler_MetricsRecording(t *testing.T) {
	tests := []struct {
		name           string
		preconditions  []configloader.Precondition
		expectedStatus string // "success", "skipped", or "failed"
		expectedErrors []string
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
			registry := prometheus.NewRegistry()
			recorder := metrics.NewRecorder("test-adapter", "v0.1.0", "test", registry)

			config := &configloader.Config{
				Adapter:       configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
				Preconditions: tt.preconditions,
			}

			exec, err := executor.NewBuilder().
				WithConfig(config).
				WithAPIClient(hyperfleetapi.NewMockClient()).
				WithTransportClient(k8sclient.NewMockK8sClient()).
				Build()
			require.NoError(t, err)

			handler := AlwaysAck(WithMetrics(CreateHandler(exec), recorder))

			evt := event.New()
			evt.SetID("test-event-1")
			evt.SetType("com.hyperfleet.test")
			evt.SetSource("test")
			eventData := map[string]interface{}{"id": "cluster-1"}
			eventBytes, _ := json.Marshal(eventData)
			_ = evt.SetData(event.ApplicationJSON, eventBytes)

			err = handler(context.Background(), &evt)
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
	registry := prometheus.NewRegistry()
	recorder := metrics.NewRecorder("test-adapter", "v0.1.0", "test", registry)

	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
		Params: []configloader.Parameter{
			{Name: "required", Source: configloader.StringSource("env.MISSING_VAR"), Required: true},
		},
	}

	exec, err := executor.NewBuilder().
		WithConfig(config).
		WithAPIClient(hyperfleetapi.NewMockClient()).
		WithTransportClient(k8sclient.NewMockK8sClient()).
		Build()
	require.NoError(t, err)

	handler := AlwaysAck(WithMetrics(CreateHandler(exec), recorder))

	evt := event.New()
	evt.SetID("test-event-fail")
	evt.SetType("com.hyperfleet.test")
	evt.SetSource("test")
	eventData := map[string]interface{}{"id": "cluster-1"}
	eventBytes, _ := json.Marshal(eventData)
	_ = evt.SetData(event.ApplicationJSON, eventBytes)

	err = handler(context.Background(), &evt)
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

	exec, err := executor.NewBuilder().
		WithConfig(config).
		WithAPIClient(hyperfleetapi.NewMockClient()).
		WithTransportClient(k8sclient.NewMockK8sClient()).
		Build()
	require.NoError(t, err)

	handler := AlwaysAck(WithMetrics(CreateHandler(exec), nil))

	evt := event.New()
	evt.SetID("test-event-nil")
	evt.SetType("com.hyperfleet.test")
	evt.SetSource("test")
	_ = evt.SetData(event.ApplicationJSON, []byte(`{"id":"cluster-1"}`))

	assert.NotPanics(t, func() {
		_ = handler(context.Background(), &evt)
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
		_ = provider.Shutdown(context.Background())
	})

	exec, err := executor.NewBuilder().
		WithConfig(&configloader.Config{
			Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
		}).
		WithAPIClient(hyperfleetapi.NewMockClient()).
		WithTransportClient(k8sclient.NewMockK8sClient()).
		Build()
	require.NoError(t, err)

	evt := event.New()
	evt.SetID("test-event-trace")
	evt.SetType("com.hyperfleet.test")
	evt.SetSource("test")
	evt.SetExtension("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	_ = evt.SetData(event.ApplicationJSON, []byte(`{"id":"cluster-1"}`))

	_, err = CreateHandler(exec)(t.Context(), &evt)
	require.NoError(t, err)

	ended := spans.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, "Execute", ended[0].Name())
	assert.Equal(t, "0af7651916cd43dd8448eb211c80319c", ended[0].SpanContext().TraceID().String())
	assert.Equal(t, "b7ad6b7169203331", ended[0].Parent().SpanID().String())
}

// TestWithMetrics_RecordsMetrics verifies WithMetrics records the correct metric status
// and passes the result through
func TestWithMetrics_RecordsMetrics(t *testing.T) {
	tests := []struct {
		result          *executor.ExecutionResult
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
			name:            "nil result",
			result:          nil,
			expectNoMetrics: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			recorder := metrics.NewRecorder("test-adapter", "v0.1.0", "test", registry)

			inner := HandlerFunc(func(_ context.Context, _ *event.Event) (*executor.ExecutionResult, error) {
				return tt.result, nil
			})
			handler := WithMetrics(inner, recorder)

			evt := event.New()
			evt.SetID("test-metrics-" + tt.name)
			evt.SetType("com.hyperfleet.test")
			evt.SetSource("test")

			got, err := handler(context.Background(), &evt)
			require.NoError(t, err)
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

	registry := prometheus.NewRegistry()
	recorder := metrics.NewRecorder("test-adapter", "v0.1.0", "test", registry)
	handler := WithMetrics(inner, recorder)

	evt := event.New()
	evt.SetID("test-handler-panic")
	evt.SetType("com.hyperfleet.test")
	evt.SetSource("test")

	assert.Panics(t, func() {
		_, _ = handler(context.Background(), &evt)
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

	evt := event.New()
	evt.SetID("test-metrics-panic")
	evt.SetType("com.hyperfleet.test")
	evt.SetSource("test")

	var got *executor.ExecutionResult
	assert.NotPanics(t, func() {
		got, _ = handler(context.Background(), &evt)
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

			evt := event.New()
			evt.SetID("test-ack")
			evt.SetType("com.hyperfleet.test")
			evt.SetSource("test")

			err := handler(context.Background(), &evt)
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
