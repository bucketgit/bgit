#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
init_output="$(init_bgit_repo gcp issues)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
run_in "$dir" admin repo issues on >/dev/null
out="$(run_in "$dir" issue create "GCP test issue" --body "created by testsuite")"
assert_contains "$out" "created issue #"
id="$(printf '%s' "$out" | sed -n 's/.*#\([0-9][0-9]*\).*/\1/p')"
assert_contains "$(run_in "$dir" issue list)" "GCP test issue"
run_in "$dir" issue comment "$id" "comment from testsuite" >/dev/null
assert_contains "$(run_in "$dir" issue view "$id")" "comment from testsuite"
run_in "$dir" issue close "$id" >/dev/null
assert_contains "$(run_in "$dir" issue view "$id")" "closed"
run_in "$dir" issue reopen "$id" >/dev/null
assert_contains "$(run_in "$dir" issue view "$id")" "open"
