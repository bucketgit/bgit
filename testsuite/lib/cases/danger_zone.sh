#!/usr/bin/env bash
set -euo pipefail
provider="$1"
source "$(dirname "$0")/../testlib.sh"

dir="$(setup_role_repo "$provider" danger)"
repo="$(git -C "$dir" config --get bucketgit.logicalRepo)"
broker="$(git -C "$dir" config --get bucketgit.broker)"
commit_file "$dir" README.md "danger zone" "initial"
run_in "$dir" push -u origin main >/dev/null

new_name="$(new_repo_name "$provider" danger-renamed)"
out="$(expect_failure_in_as admin "$dir" admin repo rename "$new_name")"
assert_contains "$out" "owner SSH signature required"
out="$(run_in "$dir" admin repo rename "$new_name")"
assert_contains "$out" "renamed repository to $new_name"
renamed_repo="$new_name.git"
assert_contains "$(git -C "$dir" config --get bucketgit.logicalRepo)" "$renamed_repo"
assert_contains "$(git -C "$dir" remote get-url origin)" "$renamed_repo"

out="$(expect_failure_in_as admin "$dir" admin repo delete --yes)"
assert_contains "$out" "owner SSH signature required"
out="$(cd "$dir" && expect_failure "$BGIT" admin repo delete)"
assert_contains "$out" "usage: bgit admin repo delete --yes"

out="$(run_in "$dir" repos mine)"
assert_contains "$out" "$renamed_repo"
out="$(run_in "$dir" admin repo delete --yes)"
assert_contains "$out" "deleted repository"
out="$(run_in "$dir" repos mine)"
assert_not_contains "$out" "$renamed_repo"
assert_not_contains "$out" "$repo"
