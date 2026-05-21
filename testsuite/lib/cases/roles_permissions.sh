#!/usr/bin/env bash
set -euo pipefail
provider="$1"
source "$(dirname "$0")/../testlib.sh"

dir="$(setup_role_repo "$provider" roles)"
commit_file "$dir" README.md "roles" "initial"
run_in "$dir" push -u origin main >/dev/null

out="$(run_in_as read "$dir" whoami --refresh)"
assert_contains "$out" "role: read"
out="$(expect_failure_in_as read "$dir" push)"
assert_contains "$out" "write SSH signature required"

out="$(run_in_as triage "$dir" issue create "triage can create issue" --body "triage")"
assert_contains "$out" "created issue #"
out="$(expect_failure_in_as triage "$dir" pr create --title "triage cannot pr" --source main --target main)"
assert_contains "$out" "write SSH signature required"

run_in_as developer "$dir" checkout -b developer/branch >/dev/null
printf 'developer\n' > "$dir/developer.txt"
run_in_as developer "$dir" add developer.txt >/dev/null
run_in_as developer "$dir" commit -m "developer branch" >/dev/null
out="$(run_in_as developer "$dir" push -u origin developer/branch)"
assert_contains "$out" "developer/branch -> developer/branch"

out="$(expect_failure_in_as maintainer "$dir" admin keys list)"
assert_contains "$out" "admin SSH signature required"
out="$(run_in_as admin "$dir" admin keys list)"
assert_contains "$out" "developer"

owner_fp="$(key_fingerprint owner)"
out="$(run_in_as admin "$dir" admin keys list)"
assert_contains "$out" $'owner\tadmin'

developer_fp="$(key_fingerprint developer)"
run_in_as admin "$dir" admin keys suspend "$developer_fp" >/dev/null
out="$(expect_failure_in_as developer "$dir" whoami --refresh)"
assert_contains "$out" "SSH signature required"
run_in_as admin "$dir" admin keys remove "$developer_fp" >/dev/null
out="$(expect_failure_in_as developer "$dir" whoami --refresh)"
assert_contains "$out" "SSH signature required"

out="$(expect_failure_in_as outsider "$dir" whoami --refresh)"
assert_contains "$out" "SSH signature required"
