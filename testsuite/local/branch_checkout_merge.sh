#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
init_output="$(init_bgit_repo gcp local-branch)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
commit_file "$dir" README.md base "base"
run_in "$dir" checkout -b feature >/dev/null
commit_file "$dir" feature.txt feature "feature"
run_in "$dir" checkout main >/dev/null
out="$(run_in "$dir" merge feature)"
assert_contains "$out" "feature.txt"
