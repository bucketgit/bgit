#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
init_output="$(init_bgit_repo gcp whoami)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
repo="$(printf "%s\n" "$init_output" | sed -n "2p")"
out="$(run_in "$dir" whoami)"
assert_contains "$out" "role"
out="$(run_in "$dir" repos mine)"
assert_contains "$out" "$repo"
