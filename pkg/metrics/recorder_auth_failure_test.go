package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorder_RecordAPIAuthFailure(t *testing.T) {
	registry := prometheus.NewRegistry()
	recorder := NewRecorder("auth-adapter", "v1.2.3", "test", registry)

	recorder.RecordAPIAuthFailure(401)
	recorder.RecordAPIAuthFailure(403)
	recorder.RecordAPIAuthFailure(401)

	families, err := registry.Gather()
	require.NoError(t, err)

	family := authFailureMetricFamily(families)
	require.NotNil(t, family, "auth failure metric family should exist")

	counts := make(map[string]float64)
	for _, metric := range family.GetMetric() {
		labels := metricLabels(metric)
		assert.Equal(t, "auth-adapter", labels["component"])
		assert.Equal(t, "v1.2.3", labels["version"])
		counts[labels["status_code"]] = metric.GetCounter().GetValue()
	}

	assert.Equal(t, float64(2), counts["401"], "401 failures must use their own bounded series")
	assert.Equal(t, float64(1), counts["403"], "403 failures must use their own bounded series")
	assert.Len(t, counts, 2, "only supported authentication status codes may create series")
}

func TestRecorder_RecordAPIAuthFailure_InvalidStatusIsNoOp(t *testing.T) {
	registry := prometheus.NewRegistry()
	recorder := NewRecorder("test-adapter", "v0.1.0", "test", registry)

	recorder.RecordAPIAuthFailure(400)
	recorder.RecordAPIAuthFailure(404)
	recorder.RecordAPIAuthFailure(500)

	families, err := registry.Gather()
	require.NoError(t, err)
	assert.Nil(t, authFailureMetricFamily(families), "non-auth status codes must not create auth metric series")
}

func authFailureMetricFamily(families []*dto.MetricFamily) *dto.MetricFamily {
	for _, family := range families {
		if family.GetName() == "hyperfleet_adapter_api_auth_failures_total" {
			return family
		}
	}
	return nil
}

func metricLabels(metric *dto.Metric) map[string]string {
	labels := make(map[string]string, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		labels[label.GetName()] = label.GetValue()
	}
	return labels
}
