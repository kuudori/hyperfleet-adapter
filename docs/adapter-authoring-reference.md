# Adapter Authoring Reference

Syntax tables, error messages and edge-case behavior for task configs. The [Adapter Authoring Guide](adapter-authoring-guide.md) teaches the concepts and links here for the details.

| Section | Contents |
|---|---|
| [CEL quick reference](#cel-quick-reference) | CEL syntax, helper functions and string functions |
| [Go template quick reference](#go-template-quick-reference) | Template variables, `if` and `range` |
| [Condition operators](#condition-operators) | The operators of structured conditions |
| [Common validation errors](#common-validation-errors) | Load-time errors and their fixes |
| [Troubleshooting](#troubleshooting) | Symptoms, causes and fixes |
| [404 handling](#404-handling) | What a 404 from the HyperFleet API does in each phase |
| [Dry-run mock files](#dry-run-mock-files) | The formats of the dry-run input files |
| [Generation write rules](#generation-write-rules) | When a remote transport writes, skips or refuses a generation |
| [Post-delete state](#post-delete-state) | `resources` and `resource_states` after a delete request |
| [Propagation policy](#propagation-policy) | What each `propagationPolicy` value does to dependent objects |
| [Delete, then apply again](#delete-then-apply-again) | A delete gate that becomes `false` again on a remote transport |
| [Deprecated precondition `api_call`](#deprecated-precondition-api_call) | The old precondition form, for existing configs |
| [Concepts changed in v2](#concepts-changed-in-v2) | v1 keys and their v2 replacements |

---

## CEL quick reference

[CEL Conventions](conventions/cel.md) is the full CEL reference. Its [Variables](conventions/cel.md#variables) table lists `resources`, `resource_states`, `adapter`, `env`, `event`, `config` and the params, with the contexts where each is available.

```cel
// Optional chaining: safe access to fields that may not exist
resources.?namespace.?status.?phase.orValue("")

// Presence check: the object is present
resource_states.?namespace.orValue("") == "present"

// Find a condition by type
status.conditions.filter(c, c.type == "Reconciled")

// Check that a condition exists with a status
status.conditions.exists(c, c.type == "Reconciled" && c.status == "True")

// Status of a condition, "Unknown" if absent
conditionStatus(conditions, "Reconciled")

// The same, without the helper
status.conditions.filter(c, c.type == "Reconciled").size() > 0
  ? status.conditions.filter(c, c.type == "Reconciled")[0].status
  : "Unknown"

// True only while the condition is True and has been for at least 300 seconds
stableFor(conditions, "Reconciled", 300)

// Seconds since the condition's last_transition_time, -1 if absent
conditionAge(conditions, "Reconciled")

// "True" if the first argument is true, "False" if the second is, "Unknown" otherwise
triState(isReady, isFailed)

// Ternary and string concatenation
condition ? "yes" : "no"
"prefix-" + resourceId + "-suffix"

// Composite status over two resources
resource_states.?namespace.orValue("") == "present"
  && resource_states.?configMap.orValue("") == "present"
  ? "True" : "False"

// JSON of a value, for debugging
toJson(resources.namespace)
```

Other helpers: `now()` returns the current time as an RFC 3339 string. `dig(map, "a.b.c")` returns a nested value, or null if any level is missing.

### String and list functions

`ext.Strings()` adds these methods on strings: `charAt`, `indexOf`, `lastIndexOf`, `lowerAscii`, `upperAscii`, `replace`, `split`, `substring`, `trim`, `join`.

```cel
resourceName.lowerAscii()
"us-east-1,us-west-2".split(",").exists(r, r == region)
resources.?myResource.?metadata.?name.orValue("").trim()
```

`ext.Lists()` adds `distinct()`, `sort()`, `sortBy()`, `slice()`, `flatten()` and `lists.range(n)`. [CEL Conventions](conventions/cel.md#list-extensions) has examples.

### Where the guide shows each pattern

- `resource_states` and why `hasValue()` is not a presence check: [Discovery](adapter-authoring-guide.md#discovery).
- Conditions for remote resources: [Status conditions for remote resources](adapter-authoring-guide.md#status-conditions-for-remote-resources).
- `is_deleting` and delete gates: [Ordering and deletion](adapter-authoring-guide.md#ordering-and-deletion).
- The `adapter.*` variables: [Report errors](adapter-authoring-guide.md#report-errors).
- The standard Health condition: [Your first task](adapter-authoring-guide.md#write-the-task-config). The Finalized condition: [Report cleanup with Finalized](adapter-authoring-guide.md#report-cleanup-with-finalized).

---

## Go template quick reference

```
{{ .variableName }}                              A param, capture or payload
{{ .resourceId | lower }}                         Lowercase
{{ now | date "2006-01-02T15:04:05Z07:00" }}     Current time (RFC 3339)
{{ .adapter.name }}                              Adapter name from the deployment config
{{ .adapter.version }}                           Adapter version from the deployment config
{{ .event.id }}                                  A field of the triggering event
{{ .config.adapter.name }}                       A field of the merged config (sensitive values are redacted)
{{ .env.REGION }}                                An environment variable
```

Go templates read params, precondition captures and payloads, plus `adapter`, `config`, `env` and `event`, with the same shapes as in CEL. Only CEL reads `resources` and `resource_states`.

Go templates appear in URLs, manifest values, payload strings, manifest files (`manifest.ref`) and block-scalar manifests (`manifest: |`).

The date layout uses the reference time `Mon Jan 2 15:04:05 MST 2006`. `2006` is the year, `01` is the month, and so on.

### `if` and `range`

`{{ if }}` and `{{ range }}` work in manifest files and in block-scalar manifests. They do not work in a plain inline manifest, because the loader parses it as YAML before it renders templates.

```yaml
{{ if .platformType }}
    hyperfleet.io/platform-type: "{{ .platformType }}"
{{ end }}

{{ if eq .environment "production" }}
    tier: "critical"
{{ else }}
    tier: "standard"
{{ end }}

{{ range $i, $subnet := .subnets }}
    subnet_{{ $subnet.id }}_name: "{{ $subnet.name }}"
    subnet_{{ $subnet.id }}_cidr: "{{ $subnet.cidr }}"
{{ end }}
```

To iterate over a list, the param must be a CEL expression that returns the list, not a string:

```yaml
params:
  - name: "subnets"
    source:
      expression: |
        has(resourceStatus.spec.platform.gcp.subnets)
          ? resourceStatus.spec.platform.gcp.subnets
          : []
```

---

## Condition operators

[Preconditions](adapter-authoring-guide.md#preconditions) explains structured conditions.

| Operator | Value type | Example |
|---|---|---|
| `equals` | any | `value: "True"` |
| `notEquals` | any | `value: "Terminating"` |
| `in` | list | `value: ["us-east-1", "us-west-2"]` |
| `notIn` | list | `value: ["deprecated-region"]` |
| `contains` | string | `value: "prod"` |
| `greaterThan` | number | `value: 0` |
| `lessThan` | number | `value: 100` |
| `exists` | none | The field must exist |

The loader rejects any other operator. For "does not exist" and inclusive comparisons, use a CEL `expression`.

---

## Common validation errors

| Error | Cause | Fix |
|---|---|---|
| `params[N].name is required`, `params[N].source is required` | A param without `name` or `source` | Add the field |
| `'field' and 'expression' are mutually exclusive` | Both `field` and `expression` on one capture or value | Use only one |
| `CEL parse error: ...` | Invalid CEL syntax | Check the parentheses and the string quoting |
| `undefined template variable "foo"` | `{{ .foo }}`, where `foo` is not a param or capture | Define it in `params` |
| `invalid operator "x", must be one of: ...` | An operator that is not in [Condition operators](#condition-operators) | Use a listed operator, or a CEL `expression` |
| `value must be a list for operator "in"` | `in` or `notIn` with a single `value` | Use a YAML list |

Every error starts with the path of the field. Structural validation stops at the first error. Semantic validation (CEL, template variables, operator values) reports all its errors together, under `validation failed with N error(s):`.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Resources skipped, `Health=False` with `ResourcesSkipped` | A precondition is not met | Check the preconditions. The cluster may not be in the expected state yet. This is often normal, and the Sentinel sends another event |
| The API rejects the status update | A stale `observed_generation` | Your adapter reports an older generation than the API stores. Take the generation from the API response, not from the event |
| `undefined template variable "foo"` | `{{ .foo }}` with no `foo` defined | Add `foo` to `params`. Check the spelling |
| `CEL parse error: ...` | Invalid CEL syntax | Check the parentheses, the string quoting and the optional chaining (`.?`) |
| `resources[N].manifest GVK <apiVersion/Kind> has no resource_plurals mapping in transport "<name>"` | The transport has no plural for a remote manifest's kind | Add `"<apiVersion>/<Kind>": <plural>` to the transport's `resource_plurals` |
| `resources[N].lifecycle.delete: selector-based lifecycle deletion is unsupported for remote transport; use discovery.by_name` | A remote resource uses `by_selectors` with `lifecycle.delete` | Use `discovery.by_name` |
| `resources[N].transport "<name>" target_cluster uses undefined variable "<var>"` | The transport's `target_cluster` names a variable the task does not define | Define the param, or fix the template |
| `resources[N].transport references unknown transport "<name>"; available: [...]` | The transport is not declared | Declare it under `transports` in the deployment config, or fix the name |
| `schema_version is required; use "2.0". v1 task configs are not supported` | The task has no `schema_version` | Add `schema_version: "2.0"` |
| `resources[N].recreate_on_change is unsupported for remote transport` | A remote resource sets `recreate_on_change` | Remove it. It works only on the local transport |
| `resources[N].discovery is required` | A resource has no `discovery` | Add `by_name` or `by_selectors` |
| `"<name>" is a reserved variable name` | A param or capture is named `adapter`, `config`, `env`, `event`, `resources` or `resource_states` | Rename it |
| `resources[N].manifest: line N: <field> must be a literal value` | `apiVersion` or `kind` is a template | Write a literal value |
| Resources phase fails with `missing hyperfleet.io/generation annotation` | A remote manifest has no generation annotation. The adapter checks it at apply time and in a dry run | Add `hyperfleet.io/generation: "{{ .generation }}"` |
| A remote resource stays `unsynced` for several passes | The mirror has not synced, or a write or delete is still in flight | Check that the store and the remote cluster side run. Do not report `False` for it; see [Status conditions for remote resources](adapter-authoring-guide.md#status-conditions-for-remote-resources) |
| Discovery returns nothing | The labels do not match, or the namespace is wrong | Check `discovery.namespace` and the labels. `by_name` is simpler |
| `observed_generation` is a string | A Go template instead of CEL | Use `expression: "generation"`, not `"{{ .generation }}"` |
| A post-action call returns 404 with error code `HYPERFLEET-NTF-000` | A wrong status endpoint path | Cluster: `/clusters/{id}/statuses`. NodePool: `/clusters/{id}/nodepools/{id}/statuses` |

---

## 404 handling

A `404 Not Found` from the HyperFleet API has a different effect in each phase:

- **Params.** There is no special handling. A failed `required` param ends the pass with status `failed`, and no post-actions run. A failed optional param stays unset, or takes its `default`.
- **Precondition `api_call`** (deprecated). The adapter separates two cases:
  - **Resource not found** (the default). The adapter treats any 404 as "the resource does not exist", including a 404 whose body a proxy removed. It sets `adapter.resourcesSkipped` to `true` and `adapter.skipReason` to `"ResourceNotFound"`, and it skips the resources phase. The pass ends with status `success`, and **no post-actions run**, so the API gets no report.
  - **Broken endpoint** (error code `HYPERFLEET-NTF-000`). No route matched the URL. The adapter treats this as a configuration error, and it reports a failure status.
- **Post-action `api_call`.** A resource-not-found 404 skips the remaining post-actions, and the pass does not fail. A broken-endpoint 404 fails the pass.

---

## Dry-run mock files

[Dry-run mode](adapter-authoring-guide.md#dry-run-mode) explains how to run a dry run. Samples are in `test/testdata/dryrun/` and in each `charts/examples/*/` directory.

### Event file (`--dry-run-event`)

A CloudEvent with the data your adapter reads:

```json
{
  "specversion": "1.0",
  "id": "abc123",
  "type": "io.hyperfleet.cluster.updated",
  "source": "/api/hyperfleet/v1/clusters/abc123",
  "data": {
    "id": "abc123",
    "kind": "Cluster",
    "href": "/api/hyperfleet/v1/clusters/abc123",
    "generation": 5
  }
}
```

### API responses (`--dry-run-api-responses`)

Mock responses, matched by HTTP method and URL regex. An entry can list several responses for an endpoint that the pass calls more than once:

```json
{
  "responses": [
    {
      "match": {
        "method": "GET",
        "urlPattern": "/api/hyperfleet/v1/clusters/.*"
      },
      "responses": [
        {
          "statusCode": 200,
          "body": {
            "id": "abc123",
            "name": "my-cluster",
            "generation": 5,
            "status": {
              "conditions": [
                { "type": "Reconciled", "status": "False" }
              ]
            }
          }
        }
      ]
    },
    {
      "match": {
        "method": "PUT",
        "urlPattern": "/api/hyperfleet/v1/clusters/.*/statuses"
      },
      "responses": [
        { "statusCode": 200, "body": {} }
      ]
    }
  ]
}
```

### Discovery overrides (`--dry-run-discovery`)

The objects that the mock cluster holds: the fields that Kubernetes adds (`uid`, `resourceVersion`, `status`), or the mirrored state of a remote resource. Each key is the **rendered `metadata.name`** of an object. Each value is a complete object with at least `apiVersion` and `kind`:

```json
{
  "abc123-remote": {
    "apiVersion": "v1",
    "kind": "Namespace",
    "metadata": {
      "name": "abc123-remote",
      "annotations": {
        "hyperfleet.io/generation": "77"
      }
    },
    "status": {
      "phase": "Active"
    }
  }
}
```

- The mock cluster loads the objects before the pass. When any resource has a `lifecycle` block, the adapter discovers every resource before the first apply: a listed object starts as `present`, and a resource with no entry starts as `confirmed_deleted`. Without a `lifecycle` block, the adapter discovers each resource only after its apply.
- For an object to be `present` before the pass, its `apiVersion` and `kind` must match the rendered manifest. For `by_name` discovery, its `metadata.name` and, for a namespaced kind, `metadata.namespace` must match the rendered `discovery.by_name` and `discovery.namespace`. For `by_selectors` discovery, the object must carry the selector labels.
- When a resource is applied, the mock finds its override by name only, not by kind or namespace. Give each object a distinct name.
- When a resource is applied, its override replaces the rendered manifest. That is how `status` and the generation annotation come from the file. To simulate a stale mirror, put an older generation in the override.
- The mock does not model the remote cluster side's delete confirmation. A dry run checks a remote delete by reading the object again, as for a local delete.
- Overrides feed local and remote resources alike.

### JSON trace keys

`--dry-run-output json` prints the trace with these top-level keys: `status`, `event`, `params`, `resources`, `discoveredResources`, `transportOperations`, `apiRequests` and `postActions`. `transportOperations` lists every `get`, `discover`, `apply` and `delete` that the mock transport saw. Each entry has the object's `kind` and `name`, and for a remote resource its `targetCluster` and `targetResource`. Add `--dry-run-verbose` to get the request bodies in `apiRequests`.

---

## Generation write rules

[The eventual-consistency contract](adapter-authoring-guide.md#the-eventual-consistency-contract-for-remote-reads) explains the generation annotation on a remote resource. Before it writes, the adapter validates the annotation and compares it with the last request it wrote:

- The same generation as the last write causes no write (`skip`), even if the mirror still shows an older generation.
- A lower generation than the last write, or than the mirror, is refused (`skip`, with a reason that starts `stale generation`). So an old event cannot roll a resource back.
- A higher generation is written (`update`). A resource that was never written is a `create`.
- If the mirror is ahead of the last write, an incoming generation equal to the mirror's is written, not refused.
- Each apply of a resource requests its mirror again, even an apply whose operation is `skip`. So a mirror removed from the store is requested on the next pass that applies the resource. The resource reads `unsynced` until the remote cluster side mirrors it again. A pass that skips the resources phase, or a create gate that skips the resource, does not request it.

---

## Post-delete state

[Delete in order](adapter-authoring-guide.md#delete-in-order) explains delete gates. After each delete request, the executor updates `resources.X` and `resource_states.X`. It checks a local delete by reading the object again. For a remote delete, it waits for the remote cluster side to confirm, because the mirror can lag:

| Situation after the delete request | `resources.X` | `resource_states.X` | Effect on dependents |
|---|---|---|---|
| Local: not found on the re-read | absent | `confirmed_deleted` | Dependents later in the list can proceed in the same pass |
| Local: still present (finalizers) | the object | `present` | Dependents wait for the next pass |
| Remote: delete confirmed and cleanup succeeded | absent | `confirmed_deleted` | Dependents later in the list can proceed in the same pass |
| Remote: delete not confirmed, the mirror still shows the object | the mirrored object | `present` | Dependents wait |
| Remote: delete not confirmed, no mirrored object | empty placeholder | `unsynced` | Dependents wait |
| Remote: delete confirmed, but cleanup of the adapter's records failed | last mirrored object, or empty placeholder | `unsynced` | Dependents wait, and the pass fails |

A cleanup failure does not cancel the confirmation, but it keeps dependent gates closed in that pass. After a confirmed deletion is cleaned up, later passes find nothing recorded for the object and report `confirmed_deleted` again.

Discovery alone never reports `confirmed_deleted` while the mirror holds the object. After the resource's own delete step in the pass, the remote cluster side's confirmation also counts, even while the mirror still shows the object.

---

## Propagation policy

[Delete with `lifecycle.delete`](adapter-authoring-guide.md#delete-with-lifecycledelete) sets `lifecycle.delete.propagationPolicy`. It controls how Kubernetes removes the objects that the deleted object owns. It has no effect on a remote transport.

| Value | Effect |
|---|---|
| `Background` (default) | Kubernetes deletes the object at once. The garbage collector deletes its dependents afterwards |
| `Foreground` | Kubernetes deletes the dependents first, then the object |
| `Orphan` | Kubernetes deletes the object and leaves its dependents |

---

## Delete, then apply again

[Delete with `lifecycle.delete`](adapter-authoring-guide.md#delete-with-lifecycledelete) explains delete gates. A delete gate accepts any CEL expression, not only `is_deleting`. So it can be `true` on one pass and `false` on a later pass while the object still exists. The usual case is a finalizer that holds the object in `Terminating`.

On a remote transport, the adapter does not apply onto that object. While the delete is unconfirmed, the store rejects the new apply. The adapter logs this warning and tries again on the next pass:

```text
ApplyResource: delete pending, retrying on next event
```

In that pass, the resource's `resource_states` entry is `unsynced`, and the mirrored object stays in `resources`.

---

## Deprecated precondition `api_call`

A precondition can still call the API directly, but the adapter logs a deprecation warning. For a new task, put the call in a param (`params[].source.api_call`) and check the values in a precondition; see [Preconditions](adapter-authoring-guide.md#preconditions). The old form, for existing configs:

```yaml
preconditions:
  - name: "checkResourceState"
    api_call:
      method: "GET"
      url: "/api/hyperfleet/v1/clusters/{{ .resourceId }}"
    capture:
      - name: "resourceNotReconciled"
        expression: |
          conditionStatus(status.conditions, "Reconciled") != "True"
      - name: "resourceReconciledTTL"
        expression: |
          stableFor(status.conditions, "Reconciled", 300)

  - name: "validationCheck"
    expression: |
      resourceNotReconciled || resourceReconciledTTL
```

A capture reads only the API response. The precondition name holds the whole response. A 404 from this call has its own rules; see [404 handling](#404-handling).

---

## Concepts changed in v2

A summary of what changed from the v1 task config, not a migration procedure. Each row names a removed or changed key and its replacement. For v1, see the [v0.3.1 guide](https://github.com/openshift-hyperfleet/hyperfleet-adapter/blob/v0.3.1/docs/adapter-authoring-guide.md).

| Removed or changed in v1 | Replacement in v2 |
|---|---|
| `transport: {client: kubernetes}` | Omit `transport`, or write `transport: kubernetes` |
| `transport: {client: maestro, maestro: {target_cluster: ...}}` | `transport: <name>`, with a `transports.<name>` entry of `type: remote` in the deployment config. Removed; the loader rejects it |
| `clients.maestro` (and the `--maestro-*` flags and `HYPERFLEET_MAESTRO_*` variables) | `stores` and `transports` in the deployment config. Removed; the loader rejects `clients.maestro`, the CLI rejects the `--maestro-*` flags, and the adapter ignores `HYPERFLEET_MAESTRO_*` variables |
| `nested_discoveries` | One resource per object, each with its own `discovery`. Removed; the loader rejects it |
| Fields copied onto nested results (`statusFeedback`, promoted keys) | The object's own `status`, read from `resources.<name>` |
| `statusFeedbackValue()` | Read `resources.<name>.status...` directly. Removed. A task that still calls it loads, but the call fails when it is evaluated (`no such overload`). A precondition or a `lifecycle.*.when` gate then reads as false, so `lifecycle.delete.when` never deletes. A payload build expression falls back to its `default`. A param is dropped, or fails the event if it is `required`. A payload or `post_action` `when` fails the event |
| `!resources.?X.hasValue()` as an absence test | `resource_states.?X.orValue("") == "confirmed_deleted"`. `hasValue()` is also `true` for the placeholder of an `unsynced` remote resource |
| `resources.?X.hasValue()` or `has(resources.X)` as a presence test | `resource_states.?X.orValue("") == "present"`. Both forms are `true` for the placeholder of an `unsynced` remote resource; on a local transport they keep working |
| No `schema_version` | `schema_version: "2.0"`. The loader rejects a task without it |
| Params named `adapter`, `config`, `env`, `event`, `resources` or `resource_states` | Rejected at load time. Rename the param |
| Templated `apiVersion` or `kind` in a manifest | Write literal values. The loader reads them without rendering and rejects a template (`apiVersion must be a literal value` or `kind must be a literal value`) |
| One manifest bundling several resources, applied atomically | One resource per object, delivered individually. Use list order and `lifecycle.*.when` gates; see [Ordering and deletion](adapter-authoring-guide.md#ordering-and-deletion) |
