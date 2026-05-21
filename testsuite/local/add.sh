#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
init_output="$(init_bgit_repo gcp local-add)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
printf 'hello\n' > "$dir/README.md"
out="$(run_in "$dir" status)"
assert_contains "$out" "Untracked files"
run_in "$dir" add README.md >/dev/null
out="$(run_in "$dir" status)"
assert_contains "$out" "Changes to be committed"
