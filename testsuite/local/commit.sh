#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
dir="$(new_workdir local commit)"
"$BGIT" init --noninteractive --repo local-commit "$dir" --profile "$GCP_PROFILE" >/dev/null
init_local_git_identity "$dir"
printf 'commit\n' > "$dir/README.md"
run_in "$dir" add README.md >/dev/null
out="$(run_in "$dir" commit -m "local commit")"
assert_contains "$out" "local commit"
out="$(run_in "$dir" log --oneline)"
assert_contains "$out" "local commit"
