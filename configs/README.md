# Configuration templates

## Adapter v2 skeleton

[`adapter-config-template.yaml`](./adapter-config-template.yaml) and
[`adapter-task-config-template.yaml`](./adapter-task-config-template.yaml) form
a matching local Kubernetes example. The task uses the quoted
`schema_version: "2.0"`; a resource without `transport` uses the local client.
For named remote transports, see the [remote examples](../charts/examples/README.md).
Task config is YAML only; deployment config supports environment and flag
overrides.
See the [configuration reference](../docs/configuration.md#task-schema-version)
for `schema_version` and named transport settings.
This skeleton covers one local task. The
[authoring guide](../docs/adapter-authoring-guide.md) covers named transports,
preconditions, captures and `by_selectors`.

## Broker configuration

This directory contains ConfigMap templates and examples for configuring the hyperfleet-adapter broker consumer.

## Files

- **`broker-configmap-pubsub-template.yaml`** - Comprehensive template with all options and documentation
- **`broker-configmap-pubsub-example.yaml`** - Simple ready-to-use example for quick start

## Quick Start

### 1. Choose Your Broker

Currently supported:
- **Google Pub/Sub** - For GCP environments
- RabbitMQ - (template can be added if needed)

### 2. Configure for Google Pub/Sub

Edit `broker-configmap-pubsub-example.yaml`:

```yaml
data:
  # Broker configuration
  BROKER_GOOGLEPUBSUB_PROJECT_ID: "your-gcp-project"
```

Also set the adapter broker settings in the deployment config:

```yaml
clients:
  broker:
    subscription_id: "your-subscription-name"
    topic: "your-topic-name"
```

### 3. Apply the ConfigMap

```bash
kubectl apply -f configs/broker-configmap-pubsub-example.yaml
```

### 4. Reference in Deployment

The adapter deployment should reference this ConfigMap using `envFrom`:

```yaml
spec:
  containers:
  - name: adapter
    envFrom:
    - configMapRef:
        name: hyperfleet-broker-pubsub
```

## Configuration Options

### Environment Variables

The hyperfleet-broker library reads configuration from environment variables:

#### Required Variables

| Variable | Description | Example |
|----------|-------------|---------|
| `BROKER_TYPE` | Broker type | `googlepubsub` |
| `BROKER_GOOGLEPUBSUB_PROJECT_ID` | GCP project ID | `my-project` |

#### Optional Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `BROKER_GOOGLEPUBSUB_TOPIC` | Topic name for publishing | - |
| `BROKER_GOOGLEPUBSUB_MAX_OUTSTANDING_MESSAGES` | Max unacked messages | `1000` |
| `BROKER_GOOGLEPUBSUB_NUM_GOROUTINES` | Pub/Sub client goroutines | `10` |
| `SUBSCRIBER_PARALLELISM` | Concurrent handlers | `1` |
| `LOG_CONFIG` | Log configuration at startup | `false` |

### Configuration File (Alternative)

Instead of environment variables, you can use a `broker.yaml` file:

```yaml
broker:
  type: googlepubsub
  googlepubsub:
    project_id: "my-project"
    subscription: "my-subscription"
    max_outstanding_messages: 1000
    num_goroutines: 10

subscriber:
  parallelism: 5
```

Mount this as a file and set `BROKER_CONFIG_FILE=/etc/broker/broker.yaml`.

## GCP Authentication

### Option 1: Workload Identity (Recommended for GKE)

```yaml
serviceAccountName: hyperfleet-adapter
# GKE will automatically inject credentials
```

### Option 2: Service Account Key

```yaml
env:
- name: GOOGLE_APPLICATION_CREDENTIALS
  value: /var/secrets/google/key.json
volumeMounts:
- name: gcp-credentials
  mountPath: /var/secrets/google
  readOnly: true
volumes:
- name: gcp-credentials
  secret:
    secretName: gcp-service-account-key
```

### Option 3: Emulator (Development Only)

```yaml
env:
- name: PUBSUB_EMULATOR_HOST
  value: "localhost:8085"
```

## Performance Tuning

### High Throughput

For processing many messages:

```yaml
BROKER_GOOGLEPUBSUB_MAX_OUTSTANDING_MESSAGES: "5000"
BROKER_GOOGLEPUBSUB_NUM_GOROUTINES: "20"
SUBSCRIBER_PARALLELISM: "10"
```

### Low Latency

For quick response times:

```yaml
BROKER_GOOGLEPUBSUB_MAX_OUTSTANDING_MESSAGES: "100"
BROKER_GOOGLEPUBSUB_NUM_GOROUTINES: "5"
SUBSCRIBER_PARALLELISM: "3"
```

### Memory Constrained

For limited memory environments:

```yaml
BROKER_GOOGLEPUBSUB_MAX_OUTSTANDING_MESSAGES: "100"
BROKER_GOOGLEPUBSUB_NUM_GOROUTINES: "2"
SUBSCRIBER_PARALLELISM: "1"
```

## Troubleshooting

### Enable Debug Logging

```yaml
LOG_CONFIG: "true"
```

This will log the complete broker configuration at startup.

### Check Credentials

```bash
# Verify service account has required permissions
kubectl exec -it <adapter-pod> -- sh
# Inside pod:
gcloud auth list
gcloud pubsub subscriptions list --project=<project-id>
```

### Test Pub/Sub Connection

```bash
# From adapter pod
gcloud pubsub subscriptions pull <subscription-name> \
  --project=<project-id> \
  --limit=1 \
  --auto-ack
```

## Required GCP Permissions

The service account needs these IAM roles:

```
roles/pubsub.subscriber  # To consume messages
roles/pubsub.viewer      # To view subscriptions
```

Or these specific permissions:

```
pubsub.subscriptions.consume
pubsub.subscriptions.get
```

## See Also

- [hyperfleet-broker Library](https://github.com/openshift-hyperfleet/hyperfleet-broker)
- [Internal broker_consumer Package](../internal/broker_consumer/README.md)
- [Integration Tests](../test/integration/broker_consumer/README.md)
- [CloudEvents Specification](https://github.com/cloudevents/spec)
