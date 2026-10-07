# Konflux build failure notifications

These PipelineRuns send a Slack alert when a completed build fails:
`hyperfleet-adapter-on-push`,
`hyperfleet-adapter-on-tag`, `hyperfleet-adapter-chart-on-push`, and
`hyperfleet-adapter-chart-on-tag`.

Each inline pipeline has a `finally` task using the Konflux
`slack-webhook-notification` bundle. The task runs only when
`$(tasks.status)` is `Failed`; successful runs do not send build alerts.
The alert names the pipeline and links the repository, commit, and failed run.
PipelineRuns rejected before starting cannot reach this task.

The task reads the `hyperfleet-slack-webhook-notification-secret` Secret,
key `hyperfleet-slack-webhook-url`, in the `hyperfleet-tenant` namespace.
HyperFleet manages this Secret in its own tenant namespace. The webhook should
target the team's `#hyperfleet-e2e-status` channel. Never copy the webhook value
into Git, logs, or an issue. The
[notification runbook](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/docs/release/operations/notifications.md)
has the shared operational context.

## Validate and troubleshoot

With an approved controlled failure, check the PipelineRun's `finally` task
status and confirm the channel receives the expected fields and working link
within a few minutes. With an approved successful run, confirm the task is
skipped and no build failure alert appears. A success may still produce a
separate release notification. If delivery fails, inspect the final TaskRun
logs, bundle resolution, and tenant Secret metadata/key presence without
reading or printing the webhook value.

## Rotate the webhook

1. Coordinate a replacement webhook with the HyperFleet team for the approved
   channel. Establish whether the release integration shares the same value.
2. Update the build tenant Secret through the team's Secret management process
   and its configuration source if one is used. If the value is shared,
   coordinate the release source update with RelEng.
3. Validate a controlled failed build alert. If shared, validate a release
   notification too. Confirm successful builds emit no build alert.
4. Revoke the old webhook only after the new delivery paths work.

## Verify pinned task bundles

Every `quay.io/konflux-ci/tekton-catalog/*` reference in the PipelineRuns is
pinned by digest. If a digest is not a Tekton bundle (for example, a SARIF scan
report that briefly held the tag upstream), every push and tag pipeline fails at
bundle resolution. The bump diff looks the same as a healthy one, so the problem
only shows up after merge.

`make verify-tekton-bundles` fetches the manifest of each pinned digest and
applies the same compliance check as the Tekton bundles resolver. It uses the
digest only, needs no credentials and downloads no layers. Install the tool with
`go install github.com/openshift-hyperfleet/hyperfleet-hooks/cmd/hyperfleet-hooks@v0.3.0`,
or set `HYPERFLEET_HOOKS=/path/to/binary`.

In CI, the required Prow job `ci/prow/verify-tekton-bundles` runs on pull
requests that change `.tekton/` or the `Makefile`, including the MintMaker
"Update Konflux references" PRs.

If it fails, re-pin the reported task to a digest that passes, for example the
one its tag points at after upstream fixes it. Check a candidate with
`skopeo inspect --raw docker://<ref-with-digest-only>`.
