# Adapter examples

Every task example uses `schema_version: "2.0"` and plain Kubernetes
manifests. A resource without a transport uses the local Kubernetes client.

| Example | Delivery | Resources |
|---|---|---|
| [kubernetes](./kubernetes/) | Local | Namespace and Job |
| [remote-two-resources](./remote-two-resources/) | One named remote transport | Namespace, then ConfigMap |

The remote example uses a shared Redis store in its installable deployment
config. Supply a Redis service and remote applier separately. The CLI dry run
records operations with a mock transport and never connects to Redis.

Render an overlay with:

```bash
helm template example charts -f charts/examples/remote-two-resources/values.yaml \
  --set image.registry=quay.io \
  --set image.repository=openshift-hyperfleet/hyperfleet-adapter \
  --set image.tag=test
```

Each example has a `dryrun.sh [create|delete]` that builds the adapter, runs one
event with mock clients, and checks the trace. It needs Go, git and `jq`.

Replace broker and image placeholders before installing. See the
[authoring guide](../../docs/adapter-authoring-guide.md) for the v2 model.
