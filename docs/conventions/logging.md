# Logging Conventions

> **Audience:** Framework developers contributing to the adapter codebase.

Uses stdlib `log/slog` directly, configured via the shared handler from `github.com/openshift-hyperfleet/hyperfleet-logger` (import alias `hfl`). No custom Logger wrapper - every package logs through `slog.Default()` / the package-level `slog.XContext` functions.

## Patterns

```go
// Basic
slog.InfoContext(ctx, "message")

// Error logging - inline attr, no context wrapping
slog.ErrorContext(ctx, "operation failed", "error", err)

// Structured fields on context (carried through call chain)
ctx = hfl.Set(ctx, logctx.ManifestWorkKey, name)

// Built-in fields (registered by hyperfleet-logger itself)
ctx = hfl.WithResourceType(ctx, "Cluster")
ctx = hfl.Set(ctx, hfl.ResourceIDKey, clusterID)
```

Messages are lowercase. Formatted `%s`/`%v`-style messages become structured `key, value` attrs instead - `fmt.Sprintf("failed for %s: %v", name, err)` becomes `"failed", "name", name, "error", err`.

## Handler setup

The handler is built once, at process startup (`cmd/adapter/main.go`'s `initLogging`), and installed via `slog.SetDefault(slog.New(handler))`:

```go
handler := hfl.NewHandler(component, version.Version,
	hfl.WithLevel(level),
	hfl.WithFormat(format),
	hfl.WithOutput(output),
	hfl.WithContextFields(logctx.ContextFields()...),
	hfl.WithStackTrace(logctx.StackTraceFilter),
)
```

Never construct a second handler mid-request or thread a logger through function parameters - everything reads the process-global default.

## Context fields

Adapter-specific typed context keys live in `internal/logctx/` (`hfl.NewKey[T](name)`), registered via `logctx.ContextFields()` at handler construction. Values set with `hfl.Set(ctx, key, val)` are automatically attached to every log record made with that context, at any depth in the call chain - this is how a field set once (e.g. `logctx.ManifestWorkKey` in `CreateManifestWork`) reaches a log call several frames down (`retryOnTransientGRPC`) without being passed explicitly.

Use a registered `logctx` key only for fields read by more than one log call, or that must propagate across a call chain. A field consumed by exactly one downstream log call is an inline attr instead - don't register a key for it.

Never repeat a registered context field (or a hyperfleet-logger default such as `event_id`, `resource_type`, `resource_id`, `version`) as an inline slog attr on a context that already carries it. The handler emits context fields and record attrs without deduplicating keys, so JSON output can contain the same key twice.

OTel trace/span IDs use the shared `logctx.WithOTelTraceContext(ctx)` helper, which wraps `hfl.WithTraceID`/`hfl.WithSpanID` (already registered as default context fields by `hyperfleet-logger`).

## Stack traces

`hfl.WithStackTrace(filter)` attaches a stack trace to a record only when `r.Level >= slog.LevelError` AND the filter returns `true`. The adapter's filter, `logctx.StackTraceFilter`, reproduces the classification `pkg/logger` used to apply: it extracts the `"error"` attr from the record and skips capture for expected operational errors (context cancellation, network errors, expected 4xx/5xx API errors, expected K8s API/resource-data errors), capturing only for unexpected ones.

## Test support

```go
// Silence logs in a test
prev := slog.Default()
slog.SetDefault(slog.New(slog.DiscardHandler))
t.Cleanup(func() { slog.SetDefault(prev) })

// Capture and assert on log output
prev := slog.Default()
var buf bytes.Buffer
slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
t.Cleanup(func() { slog.SetDefault(prev) })
```

`slog.SetDefault` is process-global - a test using it to capture output must not run under `t.Parallel()` alongside another test that also touches `slog.Default()`. Always restore the previous logger; installing `slog.DiscardHandler` in cleanup will mute later tests that still expect a configured default.

## Reference

- Handler construction: `cmd/adapter/main.go` (`initLogging`)
- Adapter-specific context keys and stack-trace filter: `internal/logctx/`
- Shared handler package: `github.com/openshift-hyperfleet/hyperfleet-logger`
