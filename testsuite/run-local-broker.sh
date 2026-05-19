#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

runtime="${1:-${BGIT_TEST_BROKER_RUNTIME:-gcp}}"
case "$runtime" in
  gcp) provider="gcp"; port="${BGIT_TEST_BROKER_PORT:-19190}" ;;
  aws) provider="aws"; port="${BGIT_TEST_BROKER_PORT:-19191}" ;;
  *) printf 'usage: ./testsuite/run-local-broker.sh gcp|aws\n' >&2; exit 2 ;;
esac

export GOCACHE="${GOCACHE:-$(go env GOCACHE 2>/dev/null || printf '/tmp/bgit-gocache')}"
export GOMODCACHE="${GOMODCACHE:-$(go env GOMODCACHE 2>/dev/null || printf '/tmp/bgit-gomodcache')}"
if [[ "${BGIT_TEST_USE_EXISTING_BINARY:-}" != "1" ]]; then
  go build -o bgit .
fi

run_id="${BGIT_TEST_RUN_ID:-$(date +%Y%m%d%H%M%S)}"
tmp_root="${TMPDIR:-${TMP:-/tmp}}"
test_root="${BGIT_TEST_LOCAL_BROKER_ROOT:-${tmp_root%/}/bgit-local-broker-${runtime}-${run_id}}"
broker_url="http://127.0.0.1:${port}"
config_path="${test_root}/home/.bgit/config.yaml"
mkdir -p "$(dirname "$config_path")"
export HOME="${test_root}/home"

cat > "$config_path" <<EOF
version: 1
identity:
  name: BucketGit Tests
  email: tests@bucketgit.local
gcp:
  profiles:
    local:
      project_id: local
      regions:
        test:
          broker_url: ${broker_url}
aws:
  profiles:
    local:
      account_id: "000000000000"
      regions:
        test:
          broker_url: ${broker_url}
EOF

PORT="$port" BROKER_TEST_ROOT="$test_root" node broker/testserver.js "$runtime" > "${test_root}/broker.log" 2>&1 &
broker_pid=$!
status=0
cleanup() {
  status=$?
  kill "$broker_pid" >/dev/null 2>&1 || true
  if [[ -n "${SSH_AGENT_PID:-}" ]]; then ssh-agent -k >/dev/null 2>&1 || true; fi
  if [[ "$status" -eq 0 && "${BGIT_TEST_KEEP_ARTIFACTS:-}" != "1" ]]; then
    rm -rf "$test_root"
    rm -rf "$ROOT/testsuite/local/repo"
    rm -rf "$ROOT/testsuite/${provider}/repo"
  else
    printf 'kept test artifacts in %s and %s\n' "$test_root" "$ROOT/testsuite/${provider}/repo" >&2
  fi
  exit "$status"
}
trap cleanup EXIT

for _ in $(seq 1 100); do
  if curl -sS "${broker_url}/health" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
curl -sS "${broker_url}/health" >/dev/null

eval "$(ssh-agent -s)" >/dev/null
chmod 600 "$ROOT/testsuite/sshkeys/owner" >/dev/null 2>&1 || true
ssh-add "$ROOT/testsuite/sshkeys/owner" >/dev/null

owner_key="$(cat "$ROOT/testsuite/sshkeys/owner.pub")"
curl -sS -X POST "${broker_url}/owners/upsert" \
  -H 'content-type: application/json' \
  --data "{\"user\":\"owner\",\"role\":\"owner\",\"public_keys\":[\"${owner_key}\"]}" >/dev/null

export BGIT="${BGIT:-$ROOT/bgit}"
export BGIT_TEST_USE_EXISTING_BINARY=1
export BGIT_TEST_RUN_ID="$run_id"
export BGIT_TEST_PROVIDER="$provider"
export BGIT_TEST_CONFIG="$config_path"
export BGIT_TEST_GCP_PROFILE="gcp:local/test"
export BGIT_TEST_AWS_PROFILE="aws:local/test"
export BGIT_TEST_IN_LOCAL_BROKER=1

printf 'Running %s tests against local %s broker at %s\n' "$provider" "$runtime" "$broker_url"
"$ROOT/testsuite/run.sh"
