#!/usr/bin/env bash
# test-config-loading.sh - Verifies that every config parameter loads correctly from
# all available forms: config file, environment variable, and CLI flag.
#
# Usage:
#   ./scripts/test-config-loading.sh [--verbose]
#
# Output: one PASS/FAIL line per test, plus a summary at the end.
# Exit code: 0 if all tests pass, 1 if any fail.

set -euo pipefail

VERBOSE=0
for arg in "$@"; do
  [[ "$arg" == "--verbose" || "$arg" == "-v" ]] && VERBOSE=1
done

# ─── Colours ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'

PASS=0; FAIL=0; declare -a ERRORS=()

pass() { echo -e "  ${GREEN}PASS${NC}  $1"; PASS=$((PASS+1)); }
fail() {
  local name="$1" pattern="$2" output="$3"
  echo -e "  ${RED}FAIL${NC}  $name"
  echo "        expected pattern: ${pattern}"
  FAIL=$((FAIL+1)); ERRORS+=("$name")
  if [[ $VERBOSE -eq 1 ]]; then
    echo "        output:"
    echo "$output" | sed 's/^/          /'
  fi
}

section() { echo -e "\n${CYAN}══ $1 ══${NC}"; }

# assert_contains <test-name> <output> <fixed-string-pattern>
assert_contains() {
  local name="$1" output="$2" pattern="$3"
  if echo "$output" | grep -qF "$pattern"; then
    pass "$name"
  else
    fail "$name" "$pattern" "$output"
  fi
}

# assert_not_contains <test-name> <output> <fixed-string-pattern>
assert_not_contains() {
  local name="$1" output="$2" pattern="$3"
  if echo "$output" | grep -qF "$pattern"; then
    fail "$name" "NOT: $pattern" "$output"
  else
    pass "$name"
  fi
}

# ─── Setup ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
ADAPTER_BIN="$(mktemp /tmp/adapter-test-XXXXXX)"
TMPDIR_TEST="$(mktemp -d)"

cleanup() { rm -f "$ADAPTER_BIN"; rm -rf "$TMPDIR_TEST"; }
trap cleanup EXIT

echo -e "${YELLOW}Building adapter binary...${NC}"
(cd "$ROOT_DIR" && go build -o "$ADAPTER_BIN" ./cmd/adapter)
echo "  Built: $ADAPTER_BIN"

# Minimal task config (required by config-dump; task params are not under test here)
TASK_CONFIG="$TMPDIR_TEST/task.yaml"
cat > "$TASK_CONFIG" <<'YAML'
schema_version: "2.0"
params: []
YAML

# ─── Config-dump wrapper ───────────────────────────────────────────────────────
# cfg_dump <adapter_config_file> [extra CLI flags...]
# Caller must set env vars in the calling environment (use subshells).
cfg_dump() {
  local config="$1"; shift
  "$ADAPTER_BIN" config-dump -c "$config" -t "$TASK_CONFIG" "$@" 2>/dev/null
}

# ─── Config file factories ────────────────────────────────────────────────────

# k8s_config <file> [extra yaml lines...]
# Creates a minimal kubernetes-transport adapter config.
# Extra args are appended verbatim after "clients:" so 2-space-indented args
# become children of clients, and 0-space-indented args become root-level keys.
k8s_config() {
  local file="$1"; shift
  {
    cat <<'YAML'
adapter:
  name: test-adapter
  version: "0.1.0"
clients:
YAML
    printf '%s\n' "$@"
  } > "$file"
}

CFG="$TMPDIR_TEST/adapter.yaml"   # reused across tests (overwritten each time)

# ─────────────────────────────────────────────────────────────────────────────
section "Adapter identity (config file only)"
# ─────────────────────────────────────────────────────────────────────────────

k8s_config "$CFG"
out=$(cfg_dump "$CFG")
assert_contains "adapter.name from file"    "$out" "name: test-adapter"
assert_contains "adapter.version from file" "$out" "version: 0.1.0"

# ─────────────────────────────────────────────────────────────────────────────
section "HyperFleet API"
# ─────────────────────────────────────────────────────────────────────────────

# base_url
k8s_config "$CFG" "  hyperfleet_api:" "    base_url: https://file-api.example.com" "    timeout: 5s"
assert_contains "api.base_url  [file]" "$(cfg_dump "$CFG")"                                          "base_url: https://file-api.example.com"
assert_contains "api.base_url  [env]"  "$(HYPERFLEET_API_BASE_URL=https://env-api.example.com cfg_dump "$CFG")"  "base_url: https://env-api.example.com"
assert_contains "api.base_url  [cli]"  "$(cfg_dump "$CFG" --hyperfleet-api-base-url=https://cli-api.example.com)" "base_url: https://cli-api.example.com"
assert_contains "api.base_url  [cli>env]" "$(HYPERFLEET_API_BASE_URL=https://env-api.example.com cfg_dump "$CFG" --hyperfleet-api-base-url=https://cli-api.example.com)" "base_url: https://cli-api.example.com"

# version
k8s_config "$CFG" "  hyperfleet_api:" "    base_url: https://base.example.com" "    timeout: 5s" "    version: file-v99"
assert_contains "api.version   [file]" "$(cfg_dump "$CFG")"                                    "version: file-v99"
assert_contains "api.version   [env]"  "$(HYPERFLEET_API_VERSION=env-v88 cfg_dump "$CFG")"     "version: env-v88"
assert_contains "api.version   [cli]"  "$(cfg_dump "$CFG" --hyperfleet-api-version=cli-v77)"   "version: cli-v77"
assert_contains "api.version   [cli>env]" "$(HYPERFLEET_API_VERSION=env-v88 cfg_dump "$CFG" --hyperfleet-api-version=cli-v77)" "version: cli-v77"

# timeout
k8s_config "$CFG" "  hyperfleet_api:" "    base_url: https://base.example.com" "    timeout: 11s"
assert_contains "api.timeout   [file]" "$(cfg_dump "$CFG")"                                  "timeout: 11s"
assert_contains "api.timeout   [env]"  "$(HYPERFLEET_API_TIMEOUT=22s cfg_dump "$CFG")"       "timeout: 22s"
assert_contains "api.timeout   [cli]"  "$(cfg_dump "$CFG" --hyperfleet-api-timeout=33s)"     "timeout: 33s"
assert_contains "api.timeout   [cli>env]" "$(HYPERFLEET_API_TIMEOUT=22s cfg_dump "$CFG" --hyperfleet-api-timeout=33s)" "timeout: 33s"

# retry_attempts
k8s_config "$CFG" "  hyperfleet_api:" "    base_url: https://base.example.com" "    timeout: 5s" "    retry_attempts: 11"
assert_contains "api.retry_attempts [file]" "$(cfg_dump "$CFG")"                                      "retry_attempts: 11"
assert_contains "api.retry_attempts [env]"  "$(HYPERFLEET_API_RETRY_ATTEMPTS=22 cfg_dump "$CFG")"     "retry_attempts: 22"
assert_contains "api.retry_attempts [cli]"  "$(cfg_dump "$CFG" --hyperfleet-api-retry=33)"            "retry_attempts: 33"
assert_contains "api.retry_attempts [cli>env]" "$(HYPERFLEET_API_RETRY_ATTEMPTS=22 cfg_dump "$CFG" --hyperfleet-api-retry=33)" "retry_attempts: 33"

# retry_backoff
k8s_config "$CFG" "  hyperfleet_api:" "    base_url: https://base.example.com" "    timeout: 5s" "    retry_backoff: linear"
assert_contains "api.retry_backoff [file]" "$(cfg_dump "$CFG")"                                        "retry_backoff: linear"
assert_contains "api.retry_backoff [env]"  "$(HYPERFLEET_API_RETRY_BACKOFF=constant cfg_dump "$CFG")"  "retry_backoff: constant"
assert_contains "api.retry_backoff [cli]"  "$(cfg_dump "$CFG" --hyperfleet-api-retry-backoff=exponential)" "retry_backoff: exponential"
assert_contains "api.retry_backoff [cli>env]" "$(HYPERFLEET_API_RETRY_BACKOFF=constant cfg_dump "$CFG" --hyperfleet-api-retry-backoff=exponential)" "retry_backoff: exponential"

# base_delay
k8s_config "$CFG" "  hyperfleet_api:" "    base_url: https://base.example.com" "    timeout: 5s" "    base_delay: 11s"
assert_contains "api.base_delay [file]" "$(cfg_dump "$CFG")"                                   "base_delay: 11s"
assert_contains "api.base_delay [env]"  "$(HYPERFLEET_API_BASE_DELAY=22s cfg_dump "$CFG")"    "base_delay: 22s"
assert_contains "api.base_delay [cli]"  "$(cfg_dump "$CFG" --hyperfleet-api-base-delay=33s)"  "base_delay: 33s"
assert_contains "api.base_delay [cli>env]" "$(HYPERFLEET_API_BASE_DELAY=22s cfg_dump "$CFG" --hyperfleet-api-base-delay=33s)" "base_delay: 33s"

# max_delay — use sub-60s values since time.Duration.String() reformats e.g. 111s → 1m51s
k8s_config "$CFG" "  hyperfleet_api:" "    base_url: https://base.example.com" "    timeout: 5s" "    max_delay: 51s"
assert_contains "api.max_delay  [file]" "$(cfg_dump "$CFG")"                                   "max_delay: 51s"
assert_contains "api.max_delay  [env]"  "$(HYPERFLEET_API_MAX_DELAY=52s cfg_dump "$CFG")"     "max_delay: 52s"
assert_contains "api.max_delay  [cli]"  "$(cfg_dump "$CFG" --hyperfleet-api-max-delay=53s)"   "max_delay: 53s"
assert_contains "api.max_delay  [cli>env]" "$(HYPERFLEET_API_MAX_DELAY=52s cfg_dump "$CFG" --hyperfleet-api-max-delay=53s)" "max_delay: 53s"

# ─────────────────────────────────────────────────────────────────────────────
section "Broker"
# ─────────────────────────────────────────────────────────────────────────────

# subscription_id — standard env var
k8s_config "$CFG" "  broker:" "    subscription_id: file-sub-id" "    topic: file-topic"
assert_contains "broker.subscription_id [file]"    "$(cfg_dump "$CFG")"                                                      "subscription_id: file-sub-id"
assert_contains "broker.subscription_id [env]"     "$(HYPERFLEET_BROKER_SUBSCRIPTION_ID=env-sub-id cfg_dump "$CFG")"         "subscription_id: env-sub-id"
assert_contains "broker.subscription_id [cli]"     "$(cfg_dump "$CFG" --broker-subscription-id=cli-sub-id)"                  "subscription_id: cli-sub-id"
assert_contains "broker.subscription_id [cli>env]" "$(HYPERFLEET_BROKER_SUBSCRIPTION_ID=env-sub-id cfg_dump "$CFG" --broker-subscription-id=cli-sub-id)" "subscription_id: cli-sub-id"

# subscription_id — legacy env var (BROKER_SUBSCRIPTION_ID without HYPERFLEET_ prefix)
assert_contains "broker.subscription_id [legacy-env]" "$(BROKER_SUBSCRIPTION_ID=legacy-sub-id cfg_dump "$CFG")" "subscription_id: legacy-sub-id"
# standard env should take precedence over legacy env
assert_contains "broker.subscription_id [std-env>legacy-env]" "$(HYPERFLEET_BROKER_SUBSCRIPTION_ID=std-sub-id BROKER_SUBSCRIPTION_ID=legacy-sub-id cfg_dump "$CFG")" "subscription_id: std-sub-id"

# topic — standard env var
assert_contains "broker.topic [file]"    "$(cfg_dump "$CFG")"                                             "topic: file-topic"
assert_contains "broker.topic [env]"     "$(HYPERFLEET_BROKER_TOPIC=env-topic cfg_dump "$CFG")"           "topic: env-topic"
assert_contains "broker.topic [cli]"     "$(cfg_dump "$CFG" --broker-topic=cli-topic)"                    "topic: cli-topic"
assert_contains "broker.topic [cli>env]" "$(HYPERFLEET_BROKER_TOPIC=env-topic cfg_dump "$CFG" --broker-topic=cli-topic)" "topic: cli-topic"

# topic — legacy env var
assert_contains "broker.topic [legacy-env]" "$(BROKER_TOPIC=legacy-topic cfg_dump "$CFG")" "topic: legacy-topic"
assert_contains "broker.topic [std-env>legacy-env]" "$(HYPERFLEET_BROKER_TOPIC=std-topic BROKER_TOPIC=legacy-topic cfg_dump "$CFG")" "topic: std-topic"

# ─────────────────────────────────────────────────────────────────────────────
section "Kubernetes"
# ─────────────────────────────────────────────────────────────────────────────

# api_version
k8s_config "$CFG" "  kubernetes:" "    api_version: file-k8s-v1"
assert_contains "kubernetes.api_version [file]"    "$(cfg_dump "$CFG")"                                              "api_version: file-k8s-v1"
assert_contains "kubernetes.api_version [env]"     "$(HYPERFLEET_KUBERNETES_API_VERSION=env-k8s-v2 cfg_dump "$CFG")" "api_version: env-k8s-v2"
assert_contains "kubernetes.api_version [cli]"     "$(cfg_dump "$CFG" --kubernetes-api-version=cli-k8s-v3)"          "api_version: cli-k8s-v3"
assert_contains "kubernetes.api_version [cli>env]" "$(HYPERFLEET_KUBERNETES_API_VERSION=env-k8s-v2 cfg_dump "$CFG" --kubernetes-api-version=cli-k8s-v3)" "api_version: cli-k8s-v3"

# kube_config_path
k8s_config "$CFG" "  kubernetes:" "    api_version: v1" "    kube_config_path: /file/kubeconfig"
assert_contains "kubernetes.kube_config_path [file]"    "$(cfg_dump "$CFG")"                                                           "kube_config_path: /file/kubeconfig"
assert_contains "kubernetes.kube_config_path [env]"     "$(HYPERFLEET_KUBERNETES_KUBE_CONFIG_PATH=/env/kubeconfig cfg_dump "$CFG")"    "kube_config_path: /env/kubeconfig"
assert_contains "kubernetes.kube_config_path [cli]"     "$(cfg_dump "$CFG" --kubernetes-kube-config-path=/cli/kubeconfig)"             "kube_config_path: /cli/kubeconfig"
assert_contains "kubernetes.kube_config_path [cli>env]" "$(HYPERFLEET_KUBERNETES_KUBE_CONFIG_PATH=/env/kubeconfig cfg_dump "$CFG" --kubernetes-kube-config-path=/cli/kubeconfig)" "kube_config_path: /cli/kubeconfig"

# qps
k8s_config "$CFG" "  kubernetes:" "    api_version: v1" "    qps: 11.5"
assert_contains "kubernetes.qps [file]"    "$(cfg_dump "$CFG")"                                     "qps: 11.5"
assert_contains "kubernetes.qps [env]"     "$(HYPERFLEET_KUBERNETES_QPS=22.5 cfg_dump "$CFG")"     "qps: 22.5"
assert_contains "kubernetes.qps [cli]"     "$(cfg_dump "$CFG" --kubernetes-qps=33.5)"              "qps: 33.5"
assert_contains "kubernetes.qps [cli>env]" "$(HYPERFLEET_KUBERNETES_QPS=22.5 cfg_dump "$CFG" --kubernetes-qps=33.5)" "qps: 33.5"

# burst
k8s_config "$CFG" "  kubernetes:" "    api_version: v1" "    burst: 11"
assert_contains "kubernetes.burst [file]"    "$(cfg_dump "$CFG")"                                  "burst: 11"
assert_contains "kubernetes.burst [env]"     "$(HYPERFLEET_KUBERNETES_BURST=22 cfg_dump "$CFG")"  "burst: 22"
assert_contains "kubernetes.burst [cli]"     "$(cfg_dump "$CFG" --kubernetes-burst=33)"           "burst: 33"
assert_contains "kubernetes.burst [cli>env]" "$(HYPERFLEET_KUBERNETES_BURST=22 cfg_dump "$CFG" --kubernetes-burst=33)" "burst: 33"

# ─────────────────────────────────────────────────────────────────────────────
section "Log"
# ─────────────────────────────────────────────────────────────────────────────

# level
k8s_config "$CFG" "log:" "  level: debug"
assert_contains "log.level [file]"    "$(cfg_dump "$CFG")"                             "level: debug"
assert_contains "log.level [env]"     "$(LOG_LEVEL=warn cfg_dump "$CFG")"              "level: warn"
assert_contains "log.level [cli]"     "$(cfg_dump "$CFG" --log-level=error)"           "level: error"
assert_contains "log.level [cli>env]" "$(LOG_LEVEL=warn cfg_dump "$CFG" --log-level=error)" "level: error"
# env overrides file
assert_contains "log.level [env>file]" "$(LOG_LEVEL=warn cfg_dump "$CFG")"             "level: warn"

# format
k8s_config "$CFG" "log:" "  format: json"
assert_contains "log.format [file]"    "$(cfg_dump "$CFG")"                              "format: json"
assert_contains "log.format [env]"     "$(LOG_FORMAT=text cfg_dump "$CFG")"             "format: text"
assert_contains "log.format [cli]"     "$(cfg_dump "$CFG" --log-format=json)"           "format: json"
assert_contains "log.format [cli>env]" "$(LOG_FORMAT=text cfg_dump "$CFG" --log-format=json)" "format: json"

# output
k8s_config "$CFG" "log:" "  output: stderr"
assert_contains "log.output [file]"    "$(cfg_dump "$CFG")"                                 "output: stderr"
assert_contains "log.output [env]"     "$(LOG_OUTPUT=stdout cfg_dump "$CFG")"              "output: stdout"
assert_contains "log.output [cli]"     "$(cfg_dump "$CFG" --log-output=stderr)"            "output: stderr"
assert_contains "log.output [cli>env]" "$(LOG_OUTPUT=stdout cfg_dump "$CFG" --log-output=stderr)" "output: stderr"

# ─────────────────────────────────────────────────────────────────────────────
section "debug_config flag"
# ─────────────────────────────────────────────────────────────────────────────

k8s_config "$CFG" "debug_config: true"
assert_contains "debug_config [file=true]"   "$(cfg_dump "$CFG")"                                       "debug_config: true"
k8s_config "$CFG"
assert_contains "debug_config [env=true]"    "$(HYPERFLEET_DEBUG_CONFIG=true cfg_dump "$CFG")"          "debug_config: true"
assert_contains "debug_config [cli=true]"    "$(cfg_dump "$CFG" --debug-config)"                        "debug_config: true"

# ─────────────────────────────────────────────────────────────────────────────
section "Priority verification (cross-parameter)"
# ─────────────────────────────────────────────────────────────────────────────
# Use api.base_url as the representative parameter for all priority checks.

k8s_config "$CFG" "  hyperfleet_api:" "    base_url: https://file.example.com" "    timeout: 5s"

assert_contains "priority: file only → file value"    "$(cfg_dump "$CFG")"                                                                                                    "base_url: https://file.example.com"
assert_contains "priority: env > file"                "$(HYPERFLEET_API_BASE_URL=https://env.example.com cfg_dump "$CFG")"                                                    "base_url: https://env.example.com"
assert_contains "priority: cli > file"                "$(cfg_dump "$CFG" --hyperfleet-api-base-url=https://cli.example.com)"                                                  "base_url: https://cli.example.com"
assert_contains "priority: cli > env"                 "$(HYPERFLEET_API_BASE_URL=https://env.example.com cfg_dump "$CFG" --hyperfleet-api-base-url=https://cli.example.com)"  "base_url: https://cli.example.com"
assert_contains "priority: cli > env > file"          "$(HYPERFLEET_API_BASE_URL=https://env.example.com cfg_dump "$CFG" --hyperfleet-api-base-url=https://cli.example.com)"  "base_url: https://cli.example.com"
# Verify env does NOT bleed into CLI-set value
assert_contains "priority: env does not override cli" "$(HYPERFLEET_API_BASE_URL=https://env.example.com cfg_dump "$CFG" --hyperfleet-api-base-url=https://cli.example.com)"  "base_url: https://cli.example.com"

# ─────────────────────────────────────────────────────────────────────────────
# Summary
# ─────────────────────────────────────────────────────────────────────────────
echo ""
echo "─────────────────────────────────────────"
TOTAL=$((PASS+FAIL))
if [[ $FAIL -eq 0 ]]; then
  echo -e "${GREEN}All $TOTAL tests passed.${NC}"
else
  echo -e "${RED}$FAIL/$TOTAL tests FAILED:${NC}"
  for e in "${ERRORS[@]}"; do
    echo "  - $e"
  done
fi
echo ""
[[ $FAIL -eq 0 ]]
