#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
dir="$(new_workdir local inspect)"
"$BGIT" init --noninteractive --repo local-inspect "$dir" --profile "$GCP_PROFILE" >/dev/null
init_local_git_identity "$dir"
commit_file "$dir" README.md one "one"
printf 'two\n' >> "$dir/README.md"
out="$(run_in "$dir" diff)"
assert_contains "$out" "+two"
run_in "$dir" add README.md >/dev/null
run_in "$dir" commit -m two >/dev/null
assert_contains "$(run_in "$dir" log --oneline)" "two"
assert_contains "$(run_in "$dir" show HEAD)" "two"
assert_contains "$(run_in "$dir" status)" "working tree clean"
