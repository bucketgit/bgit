#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
init_output="$(init_bgit_repo aws remote)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
commit_file "$dir" README.md "gcp remote" "initial"
out="$(run_in "$dir" push -u origin main)"
assert_contains "$out" "main -> main"
out="$(run_in "$dir" ls-remote)"
assert_contains "$out" "refs/heads/main"
clone="$SUITE_ROOT/aws/repo/remote-clone-$RUN_ID"
rm -rf "$clone"
expect_success "$BGIT" clone "$(git -C "$dir" config --get bucketgit.broker)/$(git -C "$dir" config --get bucketgit.logicalRepo)" "$clone" >/dev/null
init_local_git_identity "$clone"
assert_file_exists "$clone/README.md"
printf 'gcp pull\n' >> "$dir/README.md"
run_in "$dir" add README.md >/dev/null
run_in "$dir" commit -m "gcp pull source" >/dev/null
run_in "$dir" push >/dev/null
out="$(run_in "$clone" pull)"
assert_contains "$out" "README.md"
