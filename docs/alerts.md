# HyperFleet Adapter Alerts

> **Audience:** SREs setting up monitoring and alerting for the adapter.

This document provides recommended alerting rules and monitoring queries for the hyperfleet-adapter.

For the canonical list of all metrics, labels, and descriptions, see [metrics.md](metrics.md). Metrics are served on port **9090** at `/metrics`. For health endpoint documentation, see [runbook.md#health-checks](runbook.md#health-checks).

---

## Recommended Alerts

### Adapter Down

```yaml
alert: HyperFleetAdapterDown
expr: >
  hyperfleet_adapter_up == 0
  or
  absent(hyperfleet_adapter_up{component="hyperfleet-adapter"})
for: 1m
labels:
  severity: critical
annotations:
  summary: "HyperFleet Adapter is down"
  description: "Adapter {{ $labels.component }} has been down for more than 1 minute."
```

> **Note:** `hyperfleet_adapter_up` is explicitly set to 0 only during graceful shutdown. On crash (OOM, panic, node failure), the metric goes stale rather than becoming 0. The `absent()` clause covers this case. It will also fire if the metric has never been scraped (e.g., fresh Prometheus deployment) — expect initial noise until the adapter registers.

### High Event Failure Rate

```yaml
alert: HyperFleetAdapterHighFailureRate
expr: |
  sum by (component, version) (rate(hyperfleet_adapter_events_processed_total{status="failed"}[5m]))
  /
  sum by (component, version) (rate(hyperfleet_adapter_events_processed_total[5m]))
  > 0.1
for: 5m
labels:
  severity: warning
annotations:
  summary: "High event failure rate"
  description: "More than 10% of events are failing for {{ $labels.component }}."
```

### HyperFleet API Authentication or Authorization Failures

```yaml
alert: HyperFleetAdapterAPIAuthFailures
expr: |
  sum by (component, version, adapter_name, status_code) (
    increase(hyperfleet_adapter_api_auth_failures_total[5m])
  ) >= 3
labels:
  severity: critical
annotations:
  summary: "HyperFleet Adapter API authentication or authorization failures"
  description: "The HyperFleet API returned HTTP {{ $labels.status_code }} to {{ $labels.adapter_name }}. Check its service-account token, API gateway configuration, and subject allowlist. When tenant enforcement is enabled, verify tenant dimensions are present and non-empty and tenant headers are propagated."
```

**Impact:** The affected adapter cannot complete API-backed parameter extraction, preconditions, or post-actions until its identity is accepted.

**Response:** For HTTP 403 responses with tenant enforcement enabled, verify the gateway-injected tenant dimensions before changing the service-account token or subject allowlist. See [API Authentication and Authorization Failures](runbook.md#api-authentication-and-authorization-failures).

### No Events Processed (Dead Man's Switch)

```yaml
alert: HyperFleetAdapterNoEventsProcessed
expr: |
  (
    sum by (component, version) (rate(hyperfleet_adapter_events_processed_total[15m])) == 0
    and on(component, version) hyperfleet_adapter_up == 1
  )
  or
  (
    hyperfleet_adapter_up == 1
    unless on(component, version) hyperfleet_adapter_events_processed_total
  )
for: 5m
labels:
  severity: warning
annotations:
  summary: "No events processed"
  description: "Adapter {{ $labels.component }} has not processed any events in ~20 minutes."
```

> **Timing:** `rate(...[15m])` takes ~15 minutes to reach zero after the last event, plus the `for: 5m` pending period. Total delay before firing is ~20 minutes. The `unless` clause handles fresh deployments where the counter has never been incremented — it fires when the adapter is up but no events metric exists yet.

### Slow Event Processing

```yaml
alert: HyperFleetAdapterSlowProcessing
expr: |
  histogram_quantile(0.95,
    sum by (component, version, le) (
      rate(hyperfleet_adapter_event_processing_duration_seconds_bucket[5m])
    )
  ) > 60
for: 5m
labels:
  severity: warning
annotations:
  summary: "Slow event processing"
  description: "P95 event processing time exceeds 60 seconds for {{ $labels.component }}."
```

> **Note:** The `sum by (component, version, le)` aggregation merges histogram buckets across replicas before computing the quantile, giving a correct cluster-wide P95. Without this, each replica's P95 would be computed independently.

### Broker Errors

```yaml
alert: HyperFleetBrokerErrors
expr: rate(hyperfleet_broker_errors_total[5m]) > 0
for: 5m
labels:
  severity: warning
annotations:
  summary: "Broker errors detected"
  description: "Broker errors occurring for {{ $labels.component }}: {{ $labels.error_type }}."
```

### Rising Error Count by Type

```yaml
alert: HyperFleetAdapterErrorsRising
expr: rate(hyperfleet_adapter_errors_total[5m]) > 0.5
for: 5m
labels:
  severity: warning
annotations:
  summary: "Adapter errors rising"
  description: "Error rate for {{ $labels.error_type }} exceeds 0.5/s on {{ $labels.component }}."
```

For basic metric queries and examples, see [metrics.md#example-promql-queries](metrics.md#example-promql-queries).
