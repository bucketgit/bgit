#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
init_output="$(init_bgit_repo gcp local-commit)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
printf 'commit\n' > "$dir/README.md"
run_in "$dir" add README.md >/dev/null
out="$(run_in "$dir" commit -m "local commit")"
assert_contains "$out" "local commit"
out="$(run_in "$dir" log --oneline)"
assert_contains "$out" "local commit"
