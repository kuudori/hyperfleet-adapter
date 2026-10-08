# CEL Expressions

> **Audience:** Framework developers contributing to the adapter codebase. For practical CEL patterns aimed at adapter authors, see [Adapter Authoring Guide — Appendix A](../adapter-authoring-guide.md#appendix-a-cel-quick-reference).

Used in precondition expressions, lifecycle delete conditions, and post-action `when` gates.

## Variables

### Namespace availability by evaluation context

| Variable | Type | Available in | Description |
|---|---|---|---|
| _(param names)_ | any | all contexts[¹](#footnotes) | Extracted params injected as **top-level names** (eg. `resourceId`). Includes api_call result maps, event-derived values, env-derived values, and expression results. |
| _(capture names)_ | any | resources, post payloads, post_action when, payload when | Named captures from `precondition.capture` are stored in params and promoted to top-level names in all downstream contexts. |
| `resources.*` | map | resources (pre-discovery state), post payloads, post_action when, payload when | Discovered objects by alias. `present` resources expose their object; `unsynced` resources expose their last known object, or an empty placeholder when there is none; confirmed-deleted and unprocessed resources are absent. Use `resource_states` to distinguish these outcomes. |
| `resource_states.*` | string | resources (pre-discovery state), post payloads, post_action when, payload when | Discovery outcome by resource alias: `present`, `confirmed_deleted`, or `unsynced`. Use `confirmed_deleted` when a lifecycle condition must distinguish confirmed absence from an unavailable mirror. |
| `adapter.*` | map | all contexts[¹](#footnotes) | Adapter name and version, plus execution metadata. See fields below. Go templates read the same map as `{{ .adapter }}`. Execution metadata is meaningful only in post-phase expressions. During params and preconditions, `executionStatus` is always `"success"` and the error fields are empty. |
| `env.*` | map | all contexts[¹](#footnotes) | All OS environment variables accessible to the process (`env.MY_VAR`). No declaration needed. |
| `event.*` | map | all contexts[¹](#footnotes) | Full triggering event payload (`event.id`, `event.kind`, etc.). No declaration needed. |
| `config.*` | map | all contexts[¹](#footnotes) | Merged deployment and task config as a nested map with snake_case keys (`config.clients.hyperfleet_api.base_url`). The runtime redacts sensitive values, such as TLS file paths and store credentials. A param with `source: config.<path>` reads the unredacted value. |

#### Footnotes

¹ "All contexts" means: param CEL expressions, precondition expressions/conditions, lifecycle create/delete when, payload when, payload build expressions, post_action when.

#### adapter.* fields

| Field | Type | Description |
|---|---|---|
| `adapter.name` | string | adapter name from the deployment config (`adapter.name`) |
| `adapter.version` | string | adapter version from the deployment config (`adapter.version`) |
| `adapter.executionStatus` | string | `"success"` or `"failed"` |
| `adapter.resourcesSkipped` | bool | `true` when resources were intentionally skipped |
| `adapter.skipReason` | string | why resources were skipped |
| `adapter.errorReason` | string | error category if failed |
| `adapter.errorMessage` | string | error message if failed |
| `adapter.executionError` | map or null | `{phase, step, message}` for the first failure, nil otherwise |
| `adapter.resourceErrors` | map | per-resource error maps keyed by resource name |

#### Go templates

Go templates (`{{ .name }}`) read params, captures, API-call precondition responses and post payloads. They also read `adapter`, `config`, `env` and `event`, with the same shapes as in CEL. Only CEL reads `resources` and `resource_states`.

#### Capture expressions

`precondition.capture` fields and expressions read only the API response. They read its top-level fields, and the precondition name holds the whole response. They cannot read params or the variables above.

#### Reserved names

`adapter`, `config`, `env`, `event`, `resources` and `resource_states` are **reserved**. The config loader rejects a param, API-call precondition, capture or post payload that uses one of these names.

#### Optional-resource patterns

`resources` and `resource_states` are empty maps before the resources phase, so guard each access:

```cel
// Field of a resource that may be missing
resources.?x.?status.?phase.orValue("")

// Discovery outcome: "present", "confirmed_deleted" or "unsynced"
resource_states.?x.orValue("") == "confirmed_deleted"
```

`!resources.?x.hasValue()` is `false` for an unsynced resource, which keeps its last known object or an empty placeholder. Use `resource_states` for presence and lifecycle decisions.

## Custom Functions

### Utility

- `now()` — current time as RFC3339 string
- `toJson(val)` — serialize any value to JSON string
- `dig(map, "dot.path")` — safe nested map access, returns null if missing

### Domain-Specific

- `conditionStatus(conditions, type)` — returns the `status` field (`"True"`, `"False"`, etc.) of the first condition matching `type`, or `"Unknown"` if absent
- `conditionAge(conditions, type)` — returns elapsed seconds since `last_transition_time` for the matching condition, or `-1` if absent
- `stableFor(conditions, type, seconds)` — returns `true` only when `conditionStatus` is `"True"` AND `conditionAge` is at least the threshold
- `statusFeedbackValue(statusFeedback, name)` — returns `fieldValue.string` of the named Maestro statusFeedback value, or `""` if absent
- `triState(trueCond, falseCond)` — returns `"True"` when first arg is true, `"False"` when second is true, `"Unknown"` otherwise

## String Extensions

`ext.Strings()` is registered — available on string values:

`charAt`, `indexOf`, `lastIndexOf`, `lowerAscii`, `replace`, `split`, `substring`, `trim`, `upperAscii`, `join`

## List Extensions

`ext.Lists()` is registered — available on list values:

- `<list>.distinct()` — remove duplicate elements
- `<list>.sort()` — sort comparable elements (string, int, bool, …)
- `<list>.sortBy(e, keyExpr)` — sort objects by a derived key expression
- `<list>.slice(start, end)` — sub-list from `start` (inclusive) to `end` (exclusive)
- `<list>.flatten()` — recursively collapse nested lists; `flatten(depth)` limits depth
- `lists.range(n)` — generate `[0, 1, …, n-1]`

## Examples

```cel
// Post-phase: check a discovered resource reports Ready=True
resource_states.?myResource.orValue("") == "present"
  && resources.?myResource.?status.?conditions.orValue([]).exists(c, c.type == "Ready" && c.status == "True")

// Post-action gate: check execution status
adapter.?executionStatus.orValue("") == "success"

// Post-action gate: skip when resources were skipped
adapter.?resourcesSkipped.orValue(false)

// Deduplicate and sort a tag list
spec.tags.distinct().sort()

// Sort node pools by replica count, extract names
spec.node_pools.sortBy(p, p.replicas).map(p, p.name)

// Flatten condition type+status pairs into one list
status.conditions.map(c, [c.type, c.status]).flatten()
```

### Before/After: Domain-Specific Functions

```cel
// Before — double-filter pattern (12+ occurrences per adapter config):
status.conditions.filter(c, c.type == "Reconciled").size() > 0
  ? status.conditions.filter(c, c.type == "Reconciled")[0].status
  : "Unknown"

// After:
conditionStatus(conditions, "Reconciled")
```

```cel
// Before — stability window with timestamp arithmetic:
status.conditions.filter(c, c.type == "Reconciled").size() > 0
  && status.conditions.filter(c, c.type == "Reconciled")[0].status == "True"
  && (timestamp(now()) - timestamp(
       status.conditions.filter(c, c.type == "Reconciled")[0].last_transition_time
     )).getSeconds() > 300

// After:
stableFor(conditions, "Reconciled", 300)
```

```cel
// Before — statusFeedback value extraction (4-step filter chain):
statusFeedback.values.filter(v, v.name == "phase").size() > 0
  ? statusFeedback.values.filter(v, v.name == "phase")[0].fieldValue.string
  : ""

// After:
statusFeedbackValue(statusFeedback, "phase")
```

```cel
// Before — nested ternary for tri-state condition:
isReady ? "True" : (isFailed ? "False" : "Unknown")

// After:
triState(isReady, isFailed)
```

## Reference

- CEL evaluator: `internal/criteria/cel_evaluator.go`
- Custom functions registered: `internal/criteria/cel_evaluator.go:71` (`ext.Strings()`, `ext.Lists()`)
- CEL validation at config load: `internal/configloader/validator.go`
