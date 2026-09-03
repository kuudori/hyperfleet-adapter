package executor

import (
	"context"
	"log/slog"
	"time"

	"github.com/cloudevents/sdk-go/v2/event"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/logctx"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/metrics"
	hfl "github.com/openshift-hyperfleet/hyperfleet-logger"
)

// HandlerFunc is a composable event handler. Build a broker-compatible handler with:
//
//	handler := AlwaysAck(WithMetrics(exec.CreateHandler(), metricsRecorder))
type HandlerFunc func(ctx context.Context, evt *event.Event) (*ExecutionResult, error)

// WithMetrics wraps a HandlerFunc to record Prometheus metrics after execution.
// A panic in metrics recording is recovered to prevent crashing the handler.
// If recorder is nil, the handler is returned unwrapped.
func WithMetrics(h HandlerFunc, recorder *metrics.Recorder) HandlerFunc {
	if recorder == nil {
		return h
	}
	return func(ctx context.Context, evt *event.Event) (*ExecutionResult, error) {
		start := time.Now()
		result, err := h(ctx, evt)
		duration := time.Since(start)

		resultForMetrics := result
		if err != nil && (result == nil || result.Status == StatusSuccess) {
			resultForMetrics = &ExecutionResult{Status: StatusFailed}
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.ErrorContext(ctx, "panic in metrics recording (recovered)", "panic", r)
				}
			}()
			recordMetrics(recorder, resultForMetrics, duration)
		}()

		return result, err
	}
}

// AlwaysAck wraps a HandlerFunc into a broker compatible handler that always returns nil,
// preventing infinite retry loops for non-recoverable errors.
// Errors are logged at warn level before being discarded.
func AlwaysAck(h HandlerFunc) func(ctx context.Context, evt *event.Event) error {
	return func(ctx context.Context, evt *event.Event) error {
		ctx = hfl.Set(ctx, logctx.EventIDKey, evt.ID())
		result, err := h(ctx, evt)
		if err != nil {
			slog.WarnContext(ctx, "event handler error (acked)",
				"event_type", evt.Type(), "error", err)
		} else if result != nil && result.Status == StatusFailed {
			phases := make([]string, 0, len(result.Errors))
			for phase := range result.Errors {
				phases = append(phases, string(phase))
			}
			slog.WarnContext(ctx, "event handler failed (acked)",
				"event_type", evt.Type(), "failed_phases", phases)
		}
		return nil
	}
}

// recordMetrics records Prometheus metrics based on the execution result.
func recordMetrics(recorder *metrics.Recorder, result *ExecutionResult, duration time.Duration) {
	if recorder == nil {
		return
	}

	recorder.ObserveProcessingDuration(duration)

	if result == nil {
		return
	}
	for _, statusCode := range result.APIAuthFailureStatusCodes {
		recorder.RecordAPIAuthFailure(statusCode)
	}

	switch {
	case result.Status == StatusFailed:
		recorder.RecordEventProcessed("failed")
		for phase, err := range result.Errors {
			recorder.RecordError(string(phase))
			if statusCode, ok := apiAuthFailureStatusCode(err); ok {
				recorder.RecordAPIAuthFailure(statusCode)
			}
		}
	case result.ResourcesSkipped:
		recorder.RecordEventProcessed("skipped")
	default:
		recorder.RecordEventProcessed("success")
	}
}
