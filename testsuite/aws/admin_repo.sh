#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
init_output="$(init_bgit_repo aws adminrepo)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
run_in "$dir" admin repo visibility private >/dev/null
run_in "$dir" admin repo issues on >/dev/null
run_in "$dir" admin repo readonly off >/dev/null
out="$(cd "$dir" && expect_failure "$BGIT" admin repo readonly maybe)"
assert_contains "$out" "usage: bgit admin repo readonly on|off"
new_name="$(new_repo_name aws adminrepo-renamed)"
run_in "$dir" admin repo rename "$new_name" >/dev/null
assert_contains "$(git -C "$dir" config --get bucketgit.logicalRepo)" "$new_name.git"
