#!/usr/bin/env bash
set -euo pipefail
provider="$1"
source "$(dirname "$0")/../testlib.sh"

init_output="$(init_bgit_repo "$provider" public-private)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
repo="$(printf "%s\n" "$init_output" | sed -n "2p")"
broker="$(git -C "$dir" config --get bucketgit.broker)"
clone_url="${broker%/}/$repo"
commit_file "$dir" README.md "public private access" "initial"
run_in "$dir" push -u origin main >/dev/null

private_no_key="$SUITE_ROOT/$provider/repo/public-private-no-key-private-$RUN_ID"
private_unknown="$SUITE_ROOT/$provider/repo/public-private-unknown-private-$RUN_ID"
out="$(SSH_AUTH_SOCK= expect_failure "$BGIT" clone "$clone_url" "$private_no_key")"
assert_contains "$out" "broker denied read access"
out="$(with_agent_key outsider expect_failure "$BGIT" clone "$clone_url" "$private_unknown")"
assert_contains "$out" "broker denied read access"

run_in "$dir" admin repo visibility public >/dev/null

public_no_key="$SUITE_ROOT/$provider/repo/public-private-no-key-public-$RUN_ID"
public_unknown="$SUITE_ROOT/$provider/repo/public-private-unknown-public-$RUN_ID"
SSH_AUTH_SOCK= expect_success "$BGIT" clone "$clone_url" "$public_no_key" >/dev/null
assert_file_exists "$public_no_key/README.md"
assert_contains "$(cat "$public_no_key/README.md")" "public private access"
with_agent_key outsider expect_success "$BGIT" clone "$clone_url" "$public_unknown" >/dev/null
assert_file_exists "$public_unknown/README.md"
assert_contains "$(cat "$public_unknown/README.md")" "public private access"

run_in "$dir" admin repo visibility private >/dev/null
out="$(cd "$public_no_key" && SSH_AUTH_SOCK= expect_failure "$BGIT" ls-remote)"
assert_contains "$out" "read SSH signature required"
