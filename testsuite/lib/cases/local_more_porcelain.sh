#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/../testlib.sh"

init_output="$(init_bgit_repo gcp local-porcelain-more)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
commit_file "$dir" README.md "line one" "initial"

run_in "$dir" switch -c feature >/dev/null
printf 'feature\n' >> "$dir/README.md"
run_in "$dir" add README.md >/dev/null
run_in "$dir" commit -m "feature change" >/dev/null
run_in "$dir" switch main >/dev/null
run_in "$dir" cherry-pick feature >/dev/null
assert_contains "$(run_in "$dir" log --oneline)" "feature change"
run_in "$dir" revert HEAD >/dev/null
assert_contains "$(run_in "$dir" log --oneline)" "Revert"

printf 'stash\n' >> "$dir/README.md"
run_in "$dir" stash push -m "stash test" >/dev/null
assert_contains "$(run_in "$dir" stash list)" "stash@{0}"
run_in "$dir" stash pop >/dev/null 2>&1 || true
assert_contains "$(run_in "$dir" status)" "modified:"
run_in "$dir" reset --hard HEAD >/dev/null

mkdir -p "$dir/tmp"
printf 'tmp\n' > "$dir/tmp/generated.txt"
run_in "$dir" clean -f -d >/dev/null
assert_file_not_exists "$dir/tmp/generated.txt"

run_in "$dir" tag v-local >/dev/null
assert_contains "$(run_in "$dir" describe)" "v-local"
run_in "$dir" tag -v v-local >/dev/null 2>&1 || true
run_in "$dir" tag -d v-local >/dev/null
assert_not_contains "$(run_in "$dir" tag)" "v-local"

assert_contains "$(run_in "$dir" blame README.md)" "line one"
assert_contains "$(run_in "$dir" ls-tree HEAD)" "README.md"
run_in "$dir" archive --format=tar HEAD >/tmp/bgit-archive-$$.tar
test -s /tmp/bgit-archive-$$.tar || fail "archive output was empty"
rm -f /tmp/bgit-archive-$$.tar
run_in "$dir" config bucketgit.test value >/dev/null
assert_contains "$(run_in "$dir" config --get bucketgit.test)" "value"

run_in "$dir" branch delete-test >/dev/null
run_in "$dir" branch -d delete-test >/dev/null
assert_not_contains "$(run_in "$dir" branch)" "delete-test"

printf 'unsafe main\n' >> "$dir/README.md"
out="$(cd "$dir" && expect_failure "$BGIT" clean --bad-option)"
assert_contains "$out" "unsupported clean option"
