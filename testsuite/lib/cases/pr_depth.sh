#!/usr/bin/env bash
set -euo pipefail
provider="$1"
source "$(dirname "$0")/../testlib.sh"

dir="$(setup_role_repo "$provider" pr-depth)"
commit_file "$dir" README.md "base" "initial"
run_in "$dir" push -u origin main >/dev/null

run_in "$dir" checkout -b feature/pr-depth >/dev/null
printf 'change\n' >> "$dir/README.md"
printf 'new\n' > "$dir/NEW.md"
run_in "$dir" add README.md NEW.md >/dev/null
run_in "$dir" commit -m "pr depth change" >/dev/null
run_in "$dir" push -u origin feature/pr-depth >/dev/null
out="$(run_in_as developer "$dir" pr create --title "PR depth" --body "body" --source feature/pr-depth --target main)"
assert_contains "$out" "created PR #"
id="$(run_in "$dir" pr list | sed -n 's/^#\([0-9][0-9]*\).*/\1/p' | head -1)"
assert_contains "$(run_in "$dir" pr view "$id")" "status: open"
assert_contains "$(run_in "$dir" pr diff "$id")" "NEW.md"

run_in "$dir" pr close "$id" >/dev/null
assert_contains "$(run_in "$dir" pr view "$id")" "status: closed"
run_in_as maintainer "$dir" pr reopen "$id" >/dev/null
assert_contains "$(run_in "$dir" pr view "$id")" "status: open"

out="$(expect_failure_in_as triage "$dir" pr approve "$id" "looks good")"
assert_contains "$out" "write SSH signature required"
run_in_as triage "$dir" pr comment "$id" "triage comment" >/dev/null
run_in_as maintainer "$dir" pr approve "$id" "looks good" >/dev/null
out="$(run_in "$dir" pr view "$id")"
assert_contains "$out" "approvals: 1"
run_in_as developer "$dir" pr reject "$id" "needs work" >/dev/null
out="$(run_in "$dir" pr view "$id")"
assert_contains "$out" "approvals: 1"
run_in_as read "$dir" pr comment "$id" "reader comment" >/dev/null
out="$(run_in "$dir" pr view "$id")"
assert_contains "$out" "PR depth"

out="$(run_in_as maintainer "$dir" pr merge "$id" --delete-branch)"
assert_contains "$out" "merged PR #$id"
assert_contains "$(run_in "$dir" pr view "$id")" "status: merged"
out="$(run_in "$dir" ls-remote --heads)"
assert_not_contains "$out" "refs/heads/feature/pr-depth"
out="$(expect_failure_in_as maintainer "$dir" pr merge "$id")"
assert_contains "$out" "pull request is not open"
