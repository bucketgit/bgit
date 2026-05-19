#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
init_output="$(init_bgit_repo gcp pr)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
commit_file "$dir" README.md "gcp pr" "initial"
run_in "$dir" push -u origin main >/dev/null
run_in "$dir" checkout -b feature/testsuite >/dev/null
commit_file "$dir" FEATURE.md "feature" "feature commit"
run_in "$dir" push -u origin feature/testsuite >/dev/null
out="$(run_in "$dir" pr create --title "GCP testsuite PR" --body "body" --source feature/testsuite --target main)"
assert_contains "$out" "created PR #"
assert_contains "$(run_in "$dir" pr list)" "GCP testsuite PR"
id="$(run_in "$dir" pr list | sed -n 's/^#\([0-9][0-9]*\).*/\1/p' | head -1)"
assert_contains "$(run_in "$dir" pr view "$id")" "GCP testsuite PR"
assert_contains "$(run_in "$dir" pr diff "$id")" "FEATURE.md"
out="$(run_in "$dir" pr close "$id")"
assert_contains "$out" "closed"
