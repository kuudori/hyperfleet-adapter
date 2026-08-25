package logctx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"syscall"
	"testing"
	"time"

	apperrors "github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/errors"
	hfl "github.com/openshift-hyperfleet/hyperfleet-logger"
	oteltrace "go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// -----------------------------------------------------------------------------
// ContextFields()
// -----------------------------------------------------------------------------

func TestContextFields(t *testing.T) {
	fields := ContextFields()

	want := map[string]struct{}{
		"event_id":            {},
		"k8s_kind":            {},
		"k8s_name":            {},
		"k8s_namespace":       {},
		"observed_generation": {},
		"maestro_consumer":    {},
		"manifestwork":        {},
		"owner_resource_type": {},
		"owner_resource_id":   {},
	}

	if len(fields) != len(want) {
		t.Fatalf("expected %d context fields, got %d", len(want), len(fields))
	}
	for _, f := range fields {
		if _, ok := want[f.Name]; !ok {
			t.Errorf("unexpected context field %q", f.Name)
		}
		delete(want, f.Name)
	}
	for name := range want {
		t.Errorf("missing context field %q", name)
	}
}

func TestContextFieldRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		key  hfl.Key[string]
		want string
	}{
		{"event_id", EventIDKey, "evt-1"},
		{"k8s_kind", K8sKindKey, "Deployment"},
		{"k8s_name", K8sNameKey, "my-app"},
		{"k8s_namespace", K8sNamespaceKey, "default"},
		{"maestro_consumer", MaestroConsumerKey, "consumer-1"},
		{"manifestwork", ManifestWorkKey, "mw-1"},
		{"owner_resource_type", OwnerResourceTypeKey, "Cluster"},
		{"owner_resource_id", OwnerResourceIDKey, "cluster-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := hfl.Set(context.Background(), tt.key, tt.want)

			got, ok := hfl.Get(ctx, tt.key)
			if !ok {
				t.Fatalf("expected value to be present")
			}
			if got != tt.want {
				t.Errorf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestContextFieldRoundTripObservedGeneration(t *testing.T) {
	ctx := hfl.Set(context.Background(), ObservedGenerationKey, int64(42))

	got, ok := hfl.Get(ctx, ObservedGenerationKey)
	if !ok || got != int64(42) {
		t.Errorf("ObservedGenerationKey: got %d, ok=%v", got, ok)
	}
}

// -----------------------------------------------------------------------------
// WithOTelTraceContext()
// -----------------------------------------------------------------------------

func TestWithOTelTraceContextNoOpWithoutActiveSpan(t *testing.T) {
	ctx := context.Background()

	got := WithOTelTraceContext(ctx)

	if _, ok := hfl.TraceIDFromContext(got); ok {
		t.Errorf("expected no trace_id to be set on a context with no active span")
	}
	if _, ok := hfl.SpanIDFromContext(got); ok {
		t.Errorf("expected no span_id to be set on a context with no active span")
	}
}

func TestWithOTelTraceContextSetsTraceAndSpanIDFromValidSpan(t *testing.T) {
	traceID, err := oteltrace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("failed to build trace ID: %v", err)
	}
	spanID, err := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("failed to build span ID: %v", err)
	}

	spanCtx := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: oteltrace.FlagsSampled,
	})
	if !spanCtx.IsValid() {
		t.Fatalf("test setup: expected constructed span context to be valid")
	}

	ctx := oteltrace.ContextWithSpanContext(context.Background(), spanCtx)

	got := WithOTelTraceContext(ctx)

	gotTraceID, ok := hfl.TraceIDFromContext(got)
	if !ok {
		t.Fatalf("expected trace_id to be set")
	}
	if gotTraceID != traceID.String() {
		t.Errorf("expected trace_id %q, got %q", traceID.String(), gotTraceID)
	}

	gotSpanID, ok := hfl.SpanIDFromContext(got)
	if !ok {
		t.Fatalf("expected span_id to be set")
	}
	if gotSpanID != spanID.String() {
		t.Errorf("expected span_id %q, got %q", spanID.String(), gotSpanID)
	}
}

// -----------------------------------------------------------------------------
// StackTraceFilter()
// -----------------------------------------------------------------------------

// recordWithError builds an slog.Record carrying err under the "error" attribute
// key, mirroring how the runtime calls slog.ErrorContext(ctx, "msg", "error", err).
func recordWithError(err error) slog.Record {
	r := slog.NewRecord(time.Now(), slog.LevelError, "msg", 0)
	r.AddAttrs(slog.Any("error", err))
	return r
}

func TestStackTraceFilter(t *testing.T) {
	groupResource := schema.GroupResource{Group: "", Resource: "pods"}

	tests := []struct {
		record   func() slog.Record
		name     string
		expected bool
	}{
		{
			name: "no error attr present",
			record: func() slog.Record {
				return slog.NewRecord(time.Now(), slog.LevelError, "msg", 0)
			},
			expected: false,
		},
		{
			name: "error attr holding a non-error value",
			record: func() slog.Record {
				r := slog.NewRecord(time.Now(), slog.LevelError, "msg", 0)
				r.AddAttrs(slog.Any("error", "not an error"))
				return r
			},
			expected: false,
		},
		{
			name:     "context.Canceled",
			record:   func() slog.Record { return recordWithError(context.Canceled) },
			expected: false,
		},
		{
			name:     "wrapped context.Canceled",
			record:   func() slog.Record { return recordWithError(errors.Join(context.Canceled)) },
			expected: false,
		},
		{
			name:     "context.DeadlineExceeded",
			record:   func() slog.Record { return recordWithError(context.DeadlineExceeded) },
			expected: false,
		},
		{
			name:     "io.EOF",
			record:   func() slog.Record { return recordWithError(io.EOF) },
			expected: false,
		},
		{
			name:     "network error (ECONNREFUSED)",
			record:   func() slog.Record { return recordWithError(syscall.ECONNREFUSED) },
			expected: false,
		},
		{
			name: "k8s NotFound",
			record: func() slog.Record {
				return recordWithError(apierrors.NewNotFound(groupResource, "my-pod"))
			},
			expected: false,
		},
		{
			name: "k8s Conflict",
			record: func() slog.Record {
				return recordWithError(apierrors.NewConflict(groupResource, "my-pod", errors.New("conflict")))
			},
			expected: false,
		},
		{
			name: "k8s AlreadyExists",
			record: func() slog.Record {
				return recordWithError(apierrors.NewAlreadyExists(groupResource, "my-pod"))
			},
			expected: false,
		},
		{
			name: "k8s Forbidden",
			record: func() slog.Record {
				return recordWithError(apierrors.NewForbidden(groupResource, "my-pod", errors.New("forbidden")))
			},
			expected: false,
		},
		{
			name: "k8s Unauthorized",
			record: func() slog.Record {
				return recordWithError(apierrors.NewUnauthorized("unauthorized"))
			},
			expected: false,
		},
		{
			name: "k8s BadRequest",
			record: func() slog.Record {
				return recordWithError(apierrors.NewBadRequest("bad request"))
			},
			expected: false,
		},
		{
			name: "k8s Gone",
			record: func() slog.Record {
				// Intentionally exercises the deprecated IsGone path, distinct from IsResourceExpired below.
				return recordWithError(apierrors.NewGone("gone")) //nolint:staticcheck
			},
			expected: false,
		},
		{
			name: "k8s ResourceExpired",
			record: func() slog.Record {
				return recordWithError(apierrors.NewResourceExpired("expired"))
			},
			expected: false,
		},
		{
			name: "k8s ServiceUnavailable",
			record: func() slog.Record {
				return recordWithError(apierrors.NewServiceUnavailable("unavailable"))
			},
			expected: false,
		},
		{
			name: "k8s Timeout",
			record: func() slog.Record {
				return recordWithError(apierrors.NewTimeoutError("timeout", 5))
			},
			expected: false,
		},
		{
			name: "k8s TooManyRequests",
			record: func() slog.Record {
				return recordWithError(apierrors.NewTooManyRequests("too many requests", 5))
			},
			expected: false,
		},
		{
			name: "APIError NotFound (404)",
			record: func() slog.Record {
				return recordWithError(apperrors.NewAPIError(
					"GET", "http://x", 404, "404 Not Found", nil, 1, 0, errors.New("boom")))
			},
			expected: false,
		},
		{
			name: "APIError Unauthorized (401)",
			record: func() slog.Record {
				return recordWithError(apperrors.NewAPIError(
					"GET", "http://x", 401, "401 Unauthorized", nil, 1, 0, errors.New("boom")))
			},
			expected: false,
		},
		{
			name: "APIError Forbidden (403)",
			record: func() slog.Record {
				return recordWithError(apperrors.NewAPIError(
					"GET", "http://x", 403, "403 Forbidden", nil, 1, 0, errors.New("boom")))
			},
			expected: false,
		},
		{
			name: "APIError BadRequest (400)",
			record: func() slog.Record {
				return recordWithError(apperrors.NewAPIError(
					"GET", "http://x", 400, "400 Bad Request", nil, 1, 0, errors.New("boom")))
			},
			expected: false,
		},
		{
			name: "APIError Conflict (409)",
			record: func() slog.Record {
				return recordWithError(apperrors.NewAPIError("GET", "http://x", 409, "409 Conflict", nil, 1, 0, errors.New("boom")))
			},
			expected: false,
		},
		{
			name: "APIError RateLimited (429)",
			record: func() slog.Record {
				return recordWithError(apperrors.NewAPIError(
					"GET", "http://x", 429, "429 Too Many Requests", nil, 1, 0, errors.New("boom")))
			},
			expected: false,
		},
		{
			name: "APIError Timeout (408)",
			record: func() slog.Record {
				return recordWithError(apperrors.NewAPIError(
					"GET", "http://x", 408, "408 Request Timeout", nil, 1, 0, errors.New("boom")))
			},
			expected: false,
		},
		{
			// Surprising but intentional: the old isExpectedAPIError classifies any
			// 5xx as an expected/skip-worthy error via apiErr.IsServerError().
			name: "APIError ServerError (500) is treated as expected",
			record: func() slog.Record {
				return recordWithError(apperrors.NewAPIError(
					"GET", "http://x", 500, "500 Internal Server Error", nil, 1, 0, errors.New("boom")))
			},
			expected: false,
		},
		{
			// An unclassified status code (not one of the specific 4xx codes handled,
			// and not a 5xx) is the only escape hatch that still captures a trace.
			name: "APIError with unclassified status code still captures trace",
			record: func() slog.Record {
				return recordWithError(apperrors.NewAPIError(
					"GET", "http://x", 300, "300 Multiple Choices", nil, 1, 0, errors.New("boom")))
			},
			expected: true,
		},
		{
			name: "K8sResourceKeyNotFoundError",
			record: func() slog.Record {
				return recordWithError(apperrors.NewK8sResourceKeyNotFoundError("Secret", "ns", "name", "key"))
			},
			expected: false,
		},
		{
			name: "K8sInvalidPathError",
			record: func() slog.Record {
				return recordWithError(apperrors.NewK8sInvalidPathError("Secret", "bad/path", "ns/name/key"))
			},
			expected: false,
		},
		{
			name: "K8sResourceDataError",
			record: func() slog.Record {
				return recordWithError(apperrors.NewK8sResourceDataError(
					"Secret", "ns", "name", "malformed data", errors.New("boom")))
			},
			expected: false,
		},
		{
			name:     "unclassified plain error captures trace",
			record:   func() slog.Record { return recordWithError(errors.New("boom")) },
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StackTraceFilter(context.Background(), tt.record())
			if got != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}
