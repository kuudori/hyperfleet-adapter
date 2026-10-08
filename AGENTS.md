# HyperFleet Adapter

<!-- Maintainers: this file loads into every agent session (CLAUDE.md imports it, and it
imports docs/conventions/). Keep it short, link to README.md and docs/ instead of copying
their tables, and keep only what an agent can't learn by reading the Makefile and docs. -->

## What this repo is

An event-driven Kubernetes resource manager in Go. The adapter consumes CloudEvents from a message broker, runs the configured pipeline (params → preconditions → resources → post-actions), and reports status to the HyperFleet API.

Go 1.26, Cobra CLI, Viper config. `make help` lists every target, and `README.md` indexes the docs.

## Validation commands

```bash
make install-hooks    # pre-commit: secret scan, commitlint, gofmt, golangci-lint, go vet, file hygiene
make fmt              # goimports -w .
make lint             # golangci-lint, pinned in tools/go.mod, config in .golangci.yml
make test             # promtool alert tests (test/alerts/) + unit tests with -race, excluding test/
make test-integration # testcontainers + envtest suites in test/integration/ (needs Docker or Podman)
make test-helm        # helm-docs check, helm lint, then template + kubeconform for several value sets and each charts/examples/ overlay
make build            # bin/hyperfleet-adapter
```

`make test-all` runs `lint`, `test`, `test-integration` and `test-helm`. Run `make test-integration` locally before pushing when you change the config loader, the executor or a transport backend (`internal/*client/`); the Prow `presubmits-integration` job runs it too, but it is a slow feedback loop.

Dry run processes one event with mock clients and needs no broker, cluster or API: `adapter serve -c <config> -t <task> --dry-run-event event.json`. README.md's "Try Locally" has the full command. Each example in `charts/examples/` has a `dryrun.sh [create|delete]` that runs one event and checks the trace.

## Two config files

| | Deployment config | Task config |
|--|--|--|
| File | `adapter-config.yaml` | `adapter-task-config.yaml` |
| Path | `-c` / `HYPERFLEET_ADAPTER_CONFIG` | `-t` / `HYPERFLEET_TASK_CONFIG` |
| Holds | adapter identity, clients, logging, named `transports` and `stores` | `schema_version`, params, preconditions, resources, post-actions |
| Overrides | CLI flag > env var > YAML > default | none, pure YAML |

Templates live in `configs/`, and working examples in `charts/examples/`. `docs/configuration.md` is the reference for every field, flag and env var.

Every example and template task in this repo declares `schema_version: "2.0"` (some legacy Go test fixtures do not). A v2 resource may name the built-in `kubernetes` transport or a transport declared in deployment config (for example, `remote-primary` with `type: remote`); without `transport`, it uses the local Kubernetes client. Unversioned legacy tasks still load until HYPERFLEET-1504 removes them. Write new configs and fixtures as v2.

Every flag except `--dry-run-*` has an env var equivalent. When you add a deployment config override, update `viperKeyMappings` and `cliFlags` in `internal/configloader/viper_loader.go`, register the flag in `cmd/adapter/main.go` with `Env: <VAR>` in its help text, and document both in `docs/configuration.md`.

## Code conventions

@docs/conventions/logging.md
@docs/conventions/cel.md

### Error handling

`pkg/errors` provides ServiceError constructors for API-style errors with numeric codes and HTTP status:

```go
errors.NotFound("cluster %s not found", clusterID)      // → *ServiceError
errors.KubernetesError("failed to get resource: %v", err)
```

These return `*ServiceError`, not `error`. Use `.AsError()` to convert.

## Packages

- `internal/executor/`: the event pipeline (params → preconditions → resources → post-actions).
- `internal/configloader/`: loads, merges and validates both configs, including CEL compilation at load time.
- `internal/criteria/`: the CEL evaluator and the custom functions.
- `internal/transportclient/`: the apply and discovery interface shared by every backend. `internal/transportregistry/` builds the named transport clients from deployment config.
- Backends: `internal/k8sclient/` (local), `internal/desireclient/` (the `remote` transport) and `internal/maestroclient/` (legacy, removed by HYPERFLEET-1504).
- `internal/dryrun/`: the mock API client, recording transport and trace output behind `--dry-run-*`.
- `internal/logctx/`: adapter-specific log context keys and the stack-trace filter (see the logging conventions above).

## Docs that change with the code

- Deployment config field, flag or env var: `docs/configuration.md`.
- Task config or CEL behavior: `docs/adapter-authoring-guide.md`, plus `docs/conventions/cel.md` for CEL variables and functions.
- Metric: `docs/metrics.md`, and the dashboard in `charts/dashboards/` if it should show there. Alert: `docs/alerts.md` and `docs/runbook.md`. A rule copied into `test/alerts/` for promtool tests must match its doc.
- `charts/values.yaml`: annotate each key with `# --` and run `make helm-docs`. **Never edit `charts/README.md` by hand.** `verify-helm-docs` fails `test-helm` when it is stale.

## CI

Prow (`openshift/release`) runs the presubmits: `validate-commits` (commitlint), `lint`, `unit` (`make test`), `presubmits-integration` (`make test-integration`), `presubmits-images`, `helm-test` (only when `charts/` or the `Makefile` change) and an optional `risk-scorer`. Konflux (`.tekton/`) builds the image and chart on every push to `main` and on `vX.Y.Z[-rcN]` tags, and posts to Slack when a build fails. `OWNERS` enforces PR approval.

## Common gotchas

**Task config has no overrides.**
Flags and env vars change only the deployment config. A task reads the environment only through `env.*` in CEL or a param with an `env.` source.

**Naming differs by layer.**
Config YAML and CEL `config.*` use `snake_case` (`subscription_id`), Go uses `CamelCase` (`SubscriptionID`), and Helm values use `camelCase` (`subscriptionId`). Both config decoders are strict, so a wrong key fails at load. Helm silently ignores a wrong values key, because `values.schema.json` does not forbid unknown keys.

**Env var prefixes are not uniform.**
Most deployment overrides use `HYPERFLEET_`, but logging uses `LOG_LEVEL`, `LOG_FORMAT` and `LOG_OUTPUT`, and unprefixed `BROKER_SUBSCRIPTION_ID` and `BROKER_TOPIC` still work as fallbacks.

**Tracing is on in the binary and off in the chart.**
Tracing is configured only through env vars. A local `adapter serve` tries to export over OTLP unless you set `HYPERFLEET_TRACING_ENABLED=false`.

**First runs of the test targets are slow.**
`make test` builds the pinned `promtool` from the Prometheus module into `bin/`. `make test-integration` builds the envtest image with `make image-integration-test` unless `INTEGRATION_ENVTEST_IMAGE` is set, which takes minutes.

## Links

- [Architecture docs](https://github.com/openshift-hyperfleet/architecture)
- [HyperFleet API spec](https://github.com/openshift-hyperfleet/hyperfleet-api-spec)
- [Broker library](https://github.com/openshift-hyperfleet/hyperfleet-broker)
- [hyperfleet-applier](https://github.com/openshift-hyperfleet/hyperfleet-applier): applies on the target cluster what `remote` transports write to the store
- [hyperfleet-infra](https://github.com/openshift-hyperfleet/hyperfleet-infra): deploys the full stack (dev and e2e)
