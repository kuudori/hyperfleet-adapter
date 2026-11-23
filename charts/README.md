# hyperfleet-adapter

![Version: 2.3.0](https://img.shields.io/badge/Version-2.3.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.0-dev](https://img.shields.io/badge/AppVersion-0.0.0--dev-informational?style=flat-square)

HyperFleet Adapter - Event-driven adapter services for HyperFleet cluster provisioning

**Homepage:** <https://github.com/openshift-hyperfleet/hyperfleet-adapter>

## Installation

```bash
helm install hyperfleet-adapter oci://REGISTRY/hyperfleet-adapter \
  --set image.registry=REGISTRY \
  --set image.repository=ORG/hyperfleet-adapter \
  --set image.tag=<version>
```

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| HyperFleet Team | <hyperfleet-team@redhat.com> |  |

> For the full deployment guide (configuration overview, env var mapping, examples), see [Deployment Guide](../docs/deployment.md).

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| adapterConfig | object | `{"create":true,"desireStore":{"auth":{"audience":"hyperfleet-desire-store","enabled":false,"expirationSeconds":3600,"tokenPath":"/var/run/secrets/hyperfleet-desire-store/token"}},"hyperfleetApi":{"auth":{"audience":"hyperfleet-api","enabled":false,"expirationSeconds":3600,"scheme":"Bearer","tokenCacheTtl":"30s","tokenPath":"/var/run/secrets/hyperfleet/token"},"baseUrl":"http://hyperfleet-api:8000","version":"v1"},"log":{"level":"info"}}` | Adapter deployment configuration. Controls how the adapter-config ConfigMap is created. Use `adapterConfig.yaml` for inline YAML, `adapterConfig.files` for chart-packaged files, or set `create: false` and provide `configMapName` to reference an existing ConfigMap. |
| adapterConfig.create | bool | `true` | Create the adapter-config ConfigMap |
| adapterConfig.hyperfleetApi | object | `{"auth":{"audience":"hyperfleet-api","enabled":false,"expirationSeconds":3600,"scheme":"Bearer","tokenCacheTtl":"30s","tokenPath":"/var/run/secrets/hyperfleet/token"},"baseUrl":"http://hyperfleet-api:8000","version":"v1"}` | HyperFleet API connection settings injected as environment variables |
| adapterConfig.hyperfleetApi.baseUrl | string | `"http://hyperfleet-api:8000"` | API base URL (`HYPERFLEET_API_BASE_URL`) |
| adapterConfig.hyperfleetApi.version | string | `"v1"` | API version (`HYPERFLEET_API_VERSION`) |
| adapterConfig.hyperfleetApi.auth | object | `{"audience":"hyperfleet-api","enabled":false,"expirationSeconds":3600,"scheme":"Bearer","tokenCacheTtl":"30s","tokenPath":"/var/run/secrets/hyperfleet/token"}` | Token-based authentication via a Kubernetes projected service account token |
| adapterConfig.hyperfleetApi.auth.enabled | bool | `false` | Enable token auth (`HYPERFLEET_API_AUTH_TOKEN_PATH`) |
| adapterConfig.hyperfleetApi.auth.audience | string | `"hyperfleet-api"` | Service account token audience (used for the projected volume) |
| adapterConfig.hyperfleetApi.auth.tokenPath | string | `"/var/run/secrets/hyperfleet/token"` | Absolute path where the token file is mounted |
| adapterConfig.hyperfleetApi.auth.scheme | string | `"Bearer"` | Authorization header scheme used when sending the token (`HYPERFLEET_API_AUTH_SCHEME`). Defaults to `Bearer`; set to `ServiceAccount` when fronted by a gateway that differentiates machine callers from human-jwt callers. |
| adapterConfig.hyperfleetApi.auth.expirationSeconds | int | `3600` | Token lifetime in seconds for the projected service account token |
| adapterConfig.hyperfleetApi.auth.tokenCacheTtl | string | `"30s"` | How long the token is cached in memory (`HYPERFLEET_API_AUTH_TOKEN_CACHE_TTL`). Zero means re-read on every request. |
| adapterConfig.desireStore | object | `{"auth":{"audience":"hyperfleet-desire-store","enabled":false,"expirationSeconds":3600,"tokenPath":"/var/run/secrets/hyperfleet-desire-store/token"}}` | Desire Store API settings, for `stores.<name>.type: api` in the adapter config. A gateway CA bundle is mounted with `extraVolumes` and `extraVolumeMounts` and named in `stores.<name>.ca_file`. The adapter reads it at startup only, so restart the adapter after the bundle changes. |
| adapterConfig.desireStore.auth | object | `{"audience":"hyperfleet-desire-store","enabled":false,"expirationSeconds":3600,"tokenPath":"/var/run/secrets/hyperfleet-desire-store/token"}` | Projected service account token for the Desire Store API. The API takes the owner of every desire from the token subject, so each adapter release needs its own service account; rendering fails for the `default` account. |
| adapterConfig.desireStore.auth.enabled | bool | `false` | Mount the projected token |
| adapterConfig.desireStore.auth.audience | string | `"hyperfleet-desire-store"` | Service account token audience that the gateway's TokenReview checks |
| adapterConfig.desireStore.auth.tokenPath | string | `"/var/run/secrets/hyperfleet-desire-store/token"` | Absolute path where the token file is mounted. Must equal `stores.<name>.token_path`, which defaults to this path. |
| adapterConfig.desireStore.auth.expirationSeconds | int | `3600` | Token lifetime in seconds for the projected service account token |
| adapterConfig.log | object | `{"level":"info"}` | Log level for the adapter |
| adapterConfig.log.level | string | `"info"` | Log level (`debug`, `info`, `warn`, `error`) |
| adapterTaskConfig | object | `{"create":true}` | Adapter task configuration. Controls how the adapter-task-config ConfigMap is created. Supports inline YAML, chart-packaged files, or external content via `--set-file`. |
| adapterTaskConfig.create | bool | `true` | Create the adapter-task-config ConfigMap |
| affinity | object | `{}` | Affinity rules for pod scheduling |
| args | list | `["serve", "--config", "/etc/adapter/adapter-config.yaml", "--task-config", "/etc/adapter/task-config.yaml"]` | Container arguments passed to the adapter binary |
| autoscaling | object | `{"enabled":false,"maxReplicas":10,"minReplicas":1,"targetCPUUtilizationPercentage":80,"targetMemoryUtilizationPercentage":80}` | Horizontal Pod Autoscaler configuration |
| autoscaling.enabled | bool | `false` | Enable the HPA |
| autoscaling.maxReplicas | int | `10` | Maximum number of replicas |
| autoscaling.minReplicas | int | `1` | Minimum number of replicas |
| autoscaling.targetCPUUtilizationPercentage | int | `80` | Target CPU utilization percentage |
| autoscaling.targetMemoryUtilizationPercentage | int | `80` | Target memory utilization percentage |
| broker | object | `{"create":true,"googlepubsub":{"createSubscriptionIfMissing":false,"createTopicIfMissing":false,"deadLetterTopic":"","expirationTTL":"","messageRetentionDuration":"","projectId":"","subscriptionId":"","topic":""},"rabbitmq":{"exchange":"","exchangeType":"topic","queue":"","url":""},"type":""}` | Broker configuration for event consumption. Supports RabbitMQ and Google Pub/Sub. Use `broker.yaml` for inline config, or set `create: false` and provide `configMapName` to reference an existing ConfigMap. |
| broker.create | bool | `true` | Create the broker ConfigMap |
| broker.type | string | `""` | Broker type (must be `googlepubsub` or `rabbitmq`) |
| broker.googlepubsub | object | `{"createSubscriptionIfMissing":false,"createTopicIfMissing":false,"deadLetterTopic":"","expirationTTL":"","messageRetentionDuration":"","projectId":"","subscriptionId":"","topic":""}` | Google Pub/Sub configuration |
| broker.googlepubsub.projectId | string | `""` | GCP project ID |
| broker.googlepubsub.topic | string | `""` | Pub/Sub topic name |
| broker.googlepubsub.subscriptionId | string | `""` | Subscription ID |
| broker.googlepubsub.deadLetterTopic | string | `""` | Dead letter topic name |
| broker.googlepubsub.createTopicIfMissing | bool | `false` | Auto-create topic if missing (use `false` in production) |
| broker.googlepubsub.createSubscriptionIfMissing | bool | `false` | Auto-create subscription if missing (use `false` in production) |
| broker.googlepubsub.messageRetentionDuration | string | `""` | Length to retain unacknowledged messages (e.g. "1d", "12h", "604800s"). Min 10m, max 31d. |
| broker.googlepubsub.expirationTTL | string | `""` | Inactivity period before subscription is auto-deleted (e.g. "1d"). Must be "0" (never expire) or >= "1d". |
| broker.rabbitmq | object | `{"exchange":"","exchangeType":"topic","queue":"","url":""}` | RabbitMQ configuration |
| broker.rabbitmq.url | string | `""` | Connection URL |
| broker.rabbitmq.queue | string | `""` | Queue name prefix (derived from topic+subscriptionId if omitted) |
| broker.rabbitmq.exchange | string | `""` | Exchange name |
| broker.rabbitmq.exchangeType | string | `"topic"` | Exchange type |
| command | list | `["/app/adapter"]` | Container command override |
| containerPorts | list | `[{containerPort: 8080, name: http}, {containerPort: 9090, name: metrics}]` | Container ports exposed by the adapter |
| env | list | `[]` | Additional environment variables injected into the adapter container |
| extraVolumeMounts | list | `[]` | Extra volume mounts added to the adapter container |
| extraVolumes | list | `[]` | Extra volumes added to the pod |
| fullnameOverride | string | `""` | Override the full release name used in resource names |
| image.pullPolicy | string | `"Always"` | Image pull policy |
| image.registry | string | `"CHANGE_ME"` | Container image registry (no default — must be set) |
| image.repository | string | `"CHANGE_ME"` | Container image repository (no default — must be set) |
| image.tag | string | `""` | Image tag (no default — must be set via `--set image.tag=<version>`) |
| imagePullSecrets | list | `[]` | Secrets for pulling images from private registries |
| initContainers | list | `[]` | Init containers added to the pod |
| lifecycle | object | `{}` | Lifecycle hooks for the adapter container |
| livenessProbe | object | `{"enabled":true,"httpGet":{"path":"/healthz","port":8080}}` | Liveness probe configuration (per HyperFleet health-endpoints standard) |
| livenessProbe.enabled | bool | `true` | Enable the liveness probe |
| livenessProbe.httpGet.path | string | `"/healthz"` | Liveness probe path |
| livenessProbe.httpGet.port | int | `8080` | Liveness probe port |
| minReadySeconds | int | `0` | Minimum seconds a pod must be ready before considered available |
| nameOverride | string | `""` | Override the chart name used in resource names |
| nodeSelector | object | `{}` | Node selector constraints for pod scheduling |
| podAnnotations | object | `{}` | Additional annotations applied to all pods |
| podDisruptionBudget | object | `{"enabled":true,"maxUnavailable":1}` | PodDisruptionBudget configuration |
| podDisruptionBudget.enabled | bool | `true` | Enable the PDB |
| podDisruptionBudget.maxUnavailable | int | `1` | Maximum number of pods that can be unavailable during disruption |
| podLabels | object | `{}` | Additional labels applied to all pods |
| labels | object | `{}` | Custom labels applied to all resources managed by this Helm chart (Deployment, Service, ServiceAccount, ClusterRole, ClusterRoleBinding, ConfigMaps, etc.) Useful for labeling resources for cleanup, tracking, or organization. Example:   labels:     environment: production     team: platform     hyperfleet-e2e-run: e2e-20260615-102422-bb5nfo2d |
| podSecurityContext | object | `{"fsGroup":65532,"runAsNonRoot":true,"runAsUser":65532}` | Pod-level security context |
| podSecurityContext.fsGroup | int | `65532` | Filesystem group for volume mounts |
| podSecurityContext.runAsNonRoot | bool | `true` | Run all containers as non-root |
| podSecurityContext.runAsUser | int | `65532` | UID for all containers |
| priorityClassName | string | `""` | PriorityClass name for pod scheduling |
| rbac | object | `{"create":true,"rules":[]}` | RBAC configuration. Required when adapter tasks interact with the cluster (e.g. creating Jobs, updating status). |
| rbac.create | bool | `true` | Create RBAC resources (Role + RoleBinding) |
| rbac.rules | list | `[]` | Additional custom RBAC rules appended to auto-generated rules |
| readinessProbe | object | `{"enabled":true,"httpGet":{"path":"/readyz","port":8080}}` | Readiness probe configuration (per HyperFleet health-endpoints standard) |
| readinessProbe.enabled | bool | `true` | Enable the readiness probe |
| readinessProbe.httpGet.path | string | `"/readyz"` | Readiness probe path |
| readinessProbe.httpGet.port | int | `8080` | Readiness probe port |
| replicaCount | int | `1` | Number of adapter replicas |
| resources | object | `{"limits":{"cpu":"500m","memory":"512Mi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | CPU and memory resource requests and limits |
| revisionHistoryLimit | int | `10` | Number of old ReplicaSets to retain for rollback |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"seccompProfile":{"type":"RuntimeDefault"}}` | Container-level security context |
| securityContext.allowPrivilegeEscalation | bool | `false` | Disallow privilege escalation |
| securityContext.readOnlyRootFilesystem | bool | `true` | Mount root filesystem as read-only |
| serviceAccount | object | `{"annotations":{},"create":true,"name":""}` | ServiceAccount configuration |
| serviceAccount.annotations | object | `{}` | Annotations added to the ServiceAccount (e.g. for Workload Identity) |
| serviceAccount.create | bool | `true` | Create a ServiceAccount for the adapter |
| serviceAccount.name | string | `""` | Override the ServiceAccount name (defaults to the release fullname) |
| sidecarContainers | list | `[]` | Sidecar containers added to the pod |
| startupProbe | object | `{"enabled":false,"httpGet":{"path":"/healthz","port":8080}}` | Startup probe configuration (useful for slow-starting containers) |
| startupProbe.enabled | bool | `false` | Enable the startup probe |
| startupProbe.httpGet.path | string | `"/healthz"` | Startup probe path |
| startupProbe.httpGet.port | int | `8080` | Startup probe port |
| strategy | object | `{"rollingUpdate":{"maxSurge":1,"maxUnavailable":0},"type":"RollingUpdate"}` | Deployment update strategy |
| strategy.rollingUpdate.maxSurge | int | `1` | Maximum number of pods above desired count during update |
| strategy.rollingUpdate.maxUnavailable | int | `0` | Maximum number of unavailable pods during update |
| strategy.type | string | `"RollingUpdate"` | Strategy type (`RollingUpdate` or `Recreate`) |
| terminationGracePeriodSeconds | int | `30` | Termination grace period in seconds |
| tolerations | list | `[]` | Tolerations for pod scheduling |
| monitoring | object | `{"dashboard":{"enabled":false},"serviceMonitor":{"enabled":false,"honorLabels":true,"interval":"30s","labels":{},"metricRelabeling":[],"namespace":"","namespaceSelector":{},"scrapeTimeout":"10s"},"tracing":{"enabled":false,"otlpEndpoint":"","otlpProtocol":"grpc","propagators":"tracecontext,baggage","sampler":"parentbased_always_on","samplerArg":"","serviceName":"hyperfleet-adapter"}}` | Monitoring and observability configuration |
| monitoring.serviceMonitor | object | `{"enabled":false,"honorLabels":true,"interval":"30s","labels":{},"metricRelabeling":[],"namespace":"","namespaceSelector":{},"scrapeTimeout":"10s"}` | ServiceMonitor for Prometheus Operator scrape configuration. Disabled by default. On clusters without Prometheus Operator CRDs, the resource is silently skipped. |
| monitoring.serviceMonitor.enabled | bool | `false` | Create a ServiceMonitor resource |
| monitoring.serviceMonitor.interval | string | `"30s"` | Scrape interval |
| monitoring.serviceMonitor.scrapeTimeout | string | `"10s"` | Scrape timeout (must be less than interval) |
| monitoring.serviceMonitor.labels | object | `{}` | Additional labels for ServiceMonitor discovery |
| monitoring.serviceMonitor.honorLabels | bool | `true` | Honor labels from the target to avoid overwriting |
| monitoring.serviceMonitor.metricRelabeling | list | `[]` | Metric relabel configs applied before ingestion |
| monitoring.serviceMonitor.namespaceSelector | object | `{}` | Namespace selector for cross-namespace monitoring |
| monitoring.serviceMonitor.namespace | string | `""` | Override the namespace where ServiceMonitor is created (defaults to release namespace) |
| monitoring.dashboard | object | `{"enabled":false}` | Grafana dashboard provisioning via sidecar ConfigMap |
| monitoring.dashboard.enabled | bool | `false` | Create a ConfigMap with the Grafana dashboard JSON |
| monitoring.tracing | object | `{"enabled":false,"otlpEndpoint":"","otlpProtocol":"grpc","propagators":"tracecontext,baggage","sampler":"parentbased_always_on","samplerArg":"","serviceName":"hyperfleet-adapter"}` | Distributed tracing configuration (OpenTelemetry) |
| monitoring.tracing.enabled | bool | `false` | Enable trace export |
| monitoring.tracing.serviceName | string | `"hyperfleet-adapter"` | Service name reported in traces |
| monitoring.tracing.otlpEndpoint | string | `""` | OTLP exporter endpoint (traces go to stdout when empty) |
| monitoring.tracing.otlpProtocol | string | `"grpc"` | OTLP protocol (`grpc` or `http/protobuf`) |
| monitoring.tracing.sampler | string | `"parentbased_always_on"` | Sampler type |
| monitoring.tracing.samplerArg | string | `""` | Sampling rate (only used with ratio-based samplers) |
| monitoring.tracing.propagators | string | `"tracecontext,baggage"` | Context propagation formats |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs](https://github.com/norwoodj/helm-docs)
