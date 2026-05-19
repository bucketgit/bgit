#!/usr/bin/env bash
set -euo pipefail
provider="$1"
source "$(dirname "$0")/../testlib.sh"

dir="$(setup_role_repo "$provider" issue-perms)"
commit_file "$dir" README.md "issues" "initial"
run_in "$dir" push -u origin main >/dev/null

run_in "$dir" admin repo issues off >/dev/null
out="$(expect_failure_in_as triage "$dir" issue create "disabled")"
assert_contains "$out" "issues are disabled"
run_in "$dir" admin repo issues on >/dev/null

out="$(expect_failure_in_as outsider "$dir" issue create "private outsider")"
assert_contains "$out" "read SSH signature required"
out="$(run_in_as read "$dir" issue create "read issue" --body "private member")"
assert_contains "$out" "created issue #"
id="$(printf '%s' "$out" | sed -n 's/.*#\([0-9][0-9]*\).*/\1/p')"
run_in_as triage "$dir" issue comment "$id" "triage comment" >/dev/null
assert_contains "$(run_in "$dir" issue view "$id")" "triage comment"
out="$(expect_failure_in_as read "$dir" issue close "$id")"
assert_contains "$out" "write SSH signature required"
run_in_as developer "$dir" issue close "$id" >/dev/null
assert_contains "$(run_in "$dir" issue view "$id")" "closed"
run_in_as developer "$dir" issue reopen "$id" >/dev/null
assert_contains "$(run_in "$dir" issue view "$id")" "open"
out="$(expect_failure_in_as developer "$dir" issue view 99999)"
assert_contains "$out" "issue not found"

run_in "$dir" admin repo visibility public >/dev/null
out="$(run_in_no_agent "$dir" issue create "anonymous public issue" --body "anon")"
assert_contains "$out" "created issue #"
anon_id="$(printf '%s' "$out" | sed -n 's/.*#\([0-9][0-9]*\).*/\1/p')"
run_in_no_agent "$dir" issue comment "$anon_id" "anonymous comment" >/dev/null
assert_contains "$(run_in "$dir" issue view "$anon_id")" "anonymous comment"

