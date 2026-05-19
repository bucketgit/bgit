#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
dir="$(new_workdir local add)"
"$BGIT" init --noninteractive --repo local-add "$dir" --profile "$GCP_PROFILE" >/dev/null
init_local_git_identity "$dir"
printf 'hello\n' > "$dir/README.md"
out="$(run_in "$dir" status)"
assert_contains "$out" "Untracked files"
run_in "$dir" add README.md >/dev/null
out="$(run_in "$dir" status)"
assert_contains "$out" "Changes to be committed"
