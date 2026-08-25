// Package logctx defines adapter-specific typed context keys and helpers
// used to enrich log records via the shared hyperfleet-logger context field
// mechanism.
package logctx

import (
	"context"
	"log/slog"

	hfl "github.com/openshift-hyperfleet/hyperfleet-logger"
	"go.opentelemetry.io/otel/trace"
)

// Adapter-specific typed context keys.
var (
	EventIDKey            = hfl.NewKey[string]("event_id")
	K8sKindKey            = hfl.NewKey[string]("k8s_kind")
	K8sNameKey            = hfl.NewKey[string]("k8s_name")
	K8sNamespaceKey       = hfl.NewKey[string]("k8s_namespace")
	ObservedGenerationKey = hfl.NewKey[int64]("observed_generation")
	MaestroConsumerKey    = hfl.NewKey[string]("maestro_consumer")
	ManifestWorkKey       = hfl.NewKey[string]("manifestwork")
	OwnerResourceTypeKey  = hfl.NewKey[string]("owner_resource_type")
	OwnerResourceIDKey    = hfl.NewKey[string]("owner_resource_id")
)

// ContextFields returns the adapter-specific context fields to register with
// the shared logger handler via hfl.WithContextFields.
func ContextFields() []hfl.ContextField {
	return []hfl.ContextField{
		hfl.StringField(EventIDKey),
		hfl.StringField(K8sKindKey),
		hfl.StringField(K8sNameKey),
		hfl.StringField(K8sNamespaceKey),
		hfl.FieldFromKey(ObservedGenerationKey, slog.Int64Value),
		hfl.StringField(MaestroConsumerKey),
		hfl.StringField(ManifestWorkKey),
		hfl.StringField(OwnerResourceTypeKey),
		hfl.StringField(OwnerResourceIDKey),
	}
}

// WithOTelTraceContext extracts OpenTelemetry trace context (trace_id, span_id)
// from the context and adds them via the shared logger's trace/span context
// helpers for distributed tracing correlation.
// If no active span exists, returns the context unchanged.
func WithOTelTraceContext(ctx context.Context) context.Context {
	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		return ctx
	}

	if spanCtx.HasTraceID() {
		ctx = hfl.WithTraceID(ctx, spanCtx.TraceID().String())
	}
	if spanCtx.HasSpanID() {
		ctx = hfl.WithSpanID(ctx, spanCtx.SpanID().String())
	}

	return ctx
}
