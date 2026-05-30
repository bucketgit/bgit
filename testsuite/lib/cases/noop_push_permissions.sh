#!/usr/bin/env bash
set -euo pipefail
provider="$1"
source "$(dirname "$0")/../testlib.sh"

dir="$(setup_role_repo "$provider" noop-push-permissions)"
commit_file "$dir" README.md "noop push permissions" "initial"
run_in "$dir" push -u origin main >/dev/null

out="$(run_in "$dir" push)"
assert_contains "$out" "Everything up-to-date"

out="$(expect_failure_in_as read "$dir" push)"
assert_contains "$out" "write SSH signature required"

out="$(run_in_as developer "$dir" push)"
assert_contains "$out" "Everything up-to-date"
