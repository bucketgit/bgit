#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
init_output="$(init_bgit_repo aws init)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
repo="$(printf "%s\n" "$init_output" | sed -n "2p")"
assert_file_exists "$dir/.git/config"
out="$(git -C "$dir" config --get bucketgit.logicalRepo)"
assert_contains "$out" "$repo"
out="$(git -C "$dir" config --get bucketgit.broker)"
assert_contains "$out" "http"

bad_dir="$(new_workdir aws init-path-rejected)"
out="$(expect_failure "$BGIT" init --noninteractive --repo team/app --profile "$AWS_PROFILE" "${CONFIG_ARGS[@]}" "$bad_dir")"
assert_contains "$out" "logical repo names must be flat"
