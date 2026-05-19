#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [[ "${BGIT_TEST_IN_LOCAL_BROKER:-}" != "1" ]]; then
  runtimes=()
  provider_filter="${BGIT_TEST_PROVIDER:-all}"
  if [[ "$provider_filter" == "all" || "$provider_filter" == "gcp" ]]; then
    runtimes+=(gcp)
  fi
  if [[ "$provider_filter" == "all" || "$provider_filter" == "aws" ]]; then
    runtimes+=(aws)
  fi
  for runtime in "${runtimes[@]}"; do
    "$ROOT/testsuite/run-local-broker.sh" "$runtime"
  done
  exit 0
fi

export GOCACHE="${GOCACHE:-$(go env GOCACHE 2>/dev/null || printf '/tmp/bgit-gocache')}"
if [[ "${BGIT_TEST_USE_EXISTING_BINARY:-}" != "1" ]]; then
  go build -o bgit .
fi

export BGIT="${BGIT:-$ROOT/bgit}"
export BGIT_TEST_RUN_ID="${BGIT_TEST_RUN_ID:-$(date +%Y%m%d%H%M%S)}"

provider_filter="${BGIT_TEST_PROVIDER:-all}"

tests=()
while IFS= read -r -d '' file; do tests+=("$file"); done < <(find testsuite/local -name '*.sh' -type f -print0 | sort -z)
if [[ "$provider_filter" == "all" || "$provider_filter" == "gcp" ]]; then
  while IFS= read -r -d '' file; do tests+=("$file"); done < <(find testsuite/gcp -maxdepth 1 -name '*.sh' -type f -print0 | sort -z)
fi
if [[ "$provider_filter" == "all" || "$provider_filter" == "aws" ]]; then
  while IFS= read -r -d '' file; do tests+=("$file"); done < <(find testsuite/aws -maxdepth 1 -name '*.sh' -type f -print0 | sort -z)
fi

printf 'Running %d integration test files with run id %s\n' "${#tests[@]}" "$BGIT_TEST_RUN_ID"
for test in "${tests[@]}"; do
  printf '\n==> %s\n' "$test"
  bash "$test"
done

printf '\nIntegration suite passed.\n'
