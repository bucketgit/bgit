#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
dir="$(new_workdir local branch)"
"$BGIT" init --noninteractive --repo local-branch "$dir" --profile "$GCP_PROFILE" >/dev/null
init_local_git_identity "$dir"
commit_file "$dir" README.md base "base"
run_in "$dir" checkout -b feature >/dev/null
commit_file "$dir" feature.txt feature "feature"
run_in "$dir" checkout main >/dev/null
out="$(run_in "$dir" merge feature)"
assert_contains "$out" "feature.txt"
