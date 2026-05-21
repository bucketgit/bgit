#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
init_output="$(init_bgit_repo gcp local-porcelain)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
commit_file "$dir" README.md "alpha" "initial"
run_in "$dir" tag v1 >/dev/null
assert_contains "$(run_in "$dir" tag)" "v1"
assert_contains "$(run_in "$dir" grep alpha)" "README.md"
run_in "$dir" mv README.md MOVED.md >/dev/null
assert_contains "$(run_in "$dir" status)" "renamed:"
run_in "$dir" restore --staged MOVED.md >/dev/null || true
run_in "$dir" reset --hard HEAD >/dev/null
assert_file_exists "$dir/README.md"
run_in "$dir" rm README.md >/dev/null
assert_contains "$(run_in "$dir" status)" "deleted:"
run_in "$dir" reset --hard HEAD >/dev/null
assert_file_exists "$dir/README.md"
head_hash="$(run_in "$dir" rev-parse HEAD)"
[[ "${#head_hash}" -ge 40 ]] || fail "expected rev-parse HEAD to return a commit hash"
assert_contains "$(run_in "$dir" ls-files)" "README.md"
