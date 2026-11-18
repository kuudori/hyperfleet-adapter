#!/usr/bin/env bash
set -euo pipefail

#execute from root repository folder
make build
./bin/hyperfleet-adapter serve \
  --log-level debug \
  --dry-run-verbose \
  --config ./test/testdata/dryrun/dryrun-kubernetes-adapter-config.yaml \
  --task-config ./test/testdata/dryrun/cel-showcase/dryrun-cel-showcase-task-config.yaml \
  --dry-run-event ./test/testdata/dryrun/event.json \
  --dry-run-discovery ./test/testdata/dryrun/cel-showcase/dryrun-cel-showcase-discovery.json \
  --dry-run-api-responses ./test/testdata/dryrun/cel-showcase/dryrun-cel-showcase-api-responses.json
