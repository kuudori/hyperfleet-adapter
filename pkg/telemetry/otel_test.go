package telemetry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
)

// clearOtelEnv ensures all OTel env vars are cleared to prevent interference from the local shell environment.
func clearOtelEnv(t *testing.T) {
	t.Setenv(envOtelExporterOtlpEndpoint, "")
	t.Setenv(envOtelExporterOtlpTracesEndpoint, "")
	t.Setenv(envOtelExporterOtlpProtocol, "")
	t.Setenv(envOtelExporterOtlpTracesProtocol, "")
	t.Setenv(envOtelTracesSampler, "")
	t.Setenv(envOtelTracesSamplerArg, "")
}

func TestCreateExporter(t *testing.T) {
	ctx := context.Background()

	t.Run("stdout exporter when no endpoint set", func(t *testing.T) {
		clearOtelEnv(t)
		exporter, err := createExporter(ctx)
		require.NoError(t, err)
		assert.NotNil(t, exporter)
		assert.NoError(t, exporter.Shutdown(ctx))
	})

	t.Run("grpc exporter when endpoint set with default protocol", func(t *testing.T) {
		clearOtelEnv(t)
		t.Setenv(envOtelExporterOtlpEndpoint, "http://localhost:4318")
		exporter, err := createExporter(ctx)
		require.NoError(t, err)
		assert.NotNil(t, exporter)
		assert.NoError(t, exporter.Shutdown(ctx))
	})

	t.Run("grpc exporter when protocol is grpc", func(t *testing.T) {
		clearOtelEnv(t)
		t.Setenv(envOtelExporterOtlpEndpoint, "localhost:4317")
		t.Setenv(envOtelExporterOtlpProtocol, "grpc")
		exporter, err := createExporter(ctx)
		require.NoError(t, err)
		assert.NotNil(t, exporter)
		assert.NoError(t, exporter.Shutdown(ctx))
	})

	t.Run("falls back to grpc for unrecognized protocol", func(t *testing.T) {
		clearOtelEnv(t)
		t.Setenv(envOtelExporterOtlpEndpoint, "http://localhost:4318")
		t.Setenv(envOtelExporterOtlpProtocol, "unknown-protocol")
		exporter, err := createExporter(ctx)
		require.NoError(t, err)
		assert.NotNil(t, exporter)
		assert.NoError(t, exporter.Shutdown(ctx))
	})

	t.Run("traces-specific endpoint takes precedence", func(t *testing.T) {
		clearOtelEnv(t)
		t.Setenv(envOtelExporterOtlpTracesEndpoint, "http://localhost:4318")
		exporter, err := createExporter(ctx)
		require.NoError(t, err)
		assert.NotNil(t, exporter)
		assert.NoError(t, exporter.Shutdown(ctx))
	})

	t.Run("traces-specific protocol takes precedence", func(t *testing.T) {
		clearOtelEnv(t)
		t.Setenv(envOtelExporterOtlpEndpoint, "http://localhost:4318")
		t.Setenv(envOtelExporterOtlpProtocol, "grpc")
		t.Setenv(envOtelExporterOtlpTracesProtocol, "http/protobuf")
		exporter, err := createExporter(ctx)
		require.NoError(t, err)
		assert.NotNil(t, exporter)
		assert.NoError(t, exporter.Shutdown(ctx))
	})
}

func TestInitTraceProvider(t *testing.T) {
	ctx := context.Background()

	t.Run("initializes with stdout exporter when no endpoint", func(t *testing.T) {
		prevTP := otel.GetTracerProvider()
		prevProp := otel.GetTextMapPropagator()
		t.Cleanup(func() {
			otel.SetTracerProvider(prevTP)
			otel.SetTextMapPropagator(prevProp)
		})
		clearOtelEnv(t)
		tp, err := InitTraceProvider(ctx, "test-service", "0.0.1")
		require.NoError(t, err)
		require.NotNil(t, tp)
		assert.NoError(t, tp.Shutdown(ctx))
	})
	t.Run("initializes with OTLP exporter when endpoint is set", func(t *testing.T) {
		prevTP := otel.GetTracerProvider()
		prevProp := otel.GetTextMapPropagator()
		t.Cleanup(func() {
			otel.SetTracerProvider(prevTP)
			otel.SetTextMapPropagator(prevProp)
		})
		clearOtelEnv(t)
		t.Setenv(envOtelExporterOtlpEndpoint, "http://localhost:4318")
		tp, err := InitTraceProvider(ctx, "test-service", "0.0.1")
		require.NoError(t, err)
		require.NotNil(t, tp)
		assert.NoError(t, tp.Shutdown(ctx))
	})
}

func TestSelectSampler(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		samplerType string
		samplerArg  string
	}{
		{"always_on", "always_on", ""},
		{"always_off", "always_off", ""},
		{"traceidratio", "traceidratio", "0.5"},
		{"parentbased_traceidratio", "parentbased_traceidratio", "1.0"},
		{"parentbased_always_on", "parentbased_always_on", ""},
		{"parentbased_always_off", "parentbased_always_off", ""},
		{"default_when_empty", "", ""},
		{"invalid_falls_back_to_default", "garbage", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearOtelEnv(t)
			if tt.samplerType != "" {
				t.Setenv(envOtelTracesSampler, tt.samplerType)
			}
			if tt.samplerArg != "" {
				t.Setenv(envOtelTracesSamplerArg, tt.samplerArg)
			}

			sampler := selectSampler(ctx)
			assert.NotNil(t, sampler)
		})
	}
}

func TestParseSamplingRate(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		envValue string
		expected float64
	}{
		{"default_when_not_set", "", 1.0},
		{"valid_ratio", "0.5", 0.5},
		{"zero_is_valid", "0.0", 0.0},
		{"one_is_valid", "1.0", 1.0},
		{"invalid_string", "notanumber", 1.0},
		{"out_of_range_positive", "2.0", 1.0},
		{"out_of_range_negative", "-0.5", 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearOtelEnv(t)
			if tt.envValue != "" {
				t.Setenv(envOtelTracesSamplerArg, tt.envValue)
			}

			rate := parseSamplingRate(ctx)
			assert.Equal(t, tt.expected, rate)
		})
	}
}

func TestInitTraceProvider_SamplerEnvironmentVariables(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		samplerType    string
		samplerArg     string
		expectedSample bool
	}{
		{"always_on", "always_on", "", true},
		{"always_off", "always_off", "", false},
		{"traceidratio_full", "traceidratio", "1.0", true},
		{"traceidratio_zero", "traceidratio", "0.0", false},
		{"parentbased_traceidratio_full", "parentbased_traceidratio", "1.0", true},
		{"parentbased_always_on", "parentbased_always_on", "", true},
		{"parentbased_always_off", "parentbased_always_off", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prevTP := otel.GetTracerProvider()
			prevProp := otel.GetTextMapPropagator()
			t.Cleanup(func() {
				otel.SetTracerProvider(prevTP)
				otel.SetTextMapPropagator(prevProp)
			})

			clearOtelEnv(t)

			t.Setenv(envOtelTracesSampler, tt.samplerType)
			if tt.samplerArg != "" {
				t.Setenv(envOtelTracesSamplerArg, tt.samplerArg)
			}

			tp, err := InitTraceProvider(ctx, "test-service", "0.0.1")
			require.NoError(t, err)
			defer func() { assert.NoError(t, tp.Shutdown(ctx)) }()

			tracer := otel.Tracer("test")
			_, span := tracer.Start(ctx, "test-span")
			sc := span.SpanContext()

			require.True(t, sc.IsValid(), "expected a valid SpanContext from initialized tracer provider")
			assert.Equal(t, tt.expectedSample, sc.TraceFlags().IsSampled())

			span.End()
		})
	}
}
