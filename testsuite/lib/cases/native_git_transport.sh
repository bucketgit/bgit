#!/usr/bin/env bash
set -euo pipefail
provider="$1"
source "$(dirname "$0")/../testlib.sh"

init_output="$(init_bgit_repo "$provider" native)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
repo="$(printf "%s\n" "$init_output" | sed -n "2p")"
commit_file "$dir" README.md "native" "initial"
(cd "$dir" && git push -u origin main >/dev/null)
assert_contains "$(run_in "$dir" ls-remote --heads)" "refs/heads/main"

clone="$SUITE_ROOT/$provider/repo/native-clone-$RUN_ID"
rm -rf "$clone"
"$BGIT" clone "$(git -C "$dir" config --get bucketgit.broker)/$repo" "$clone" >/dev/null
init_local_git_identity "$clone"
assert_file_exists "$clone/README.md"
(cd "$clone" && git fetch origin >/dev/null)
(cd "$clone" && git ls-remote origin >/tmp/bgit-native-lsremote.$$)
assert_contains "$(cat /tmp/bgit-native-lsremote.$$)" "refs/heads/main"
rm -f /tmp/bgit-native-lsremote.$$

(cd "$clone" && git checkout -b native/feature >/dev/null)
printf 'feature\n' > "$clone/native.txt"
(cd "$clone" && git add native.txt && git commit -m "native feature" >/dev/null && git push -u origin native/feature >/dev/null)
assert_contains "$(run_in "$dir" ls-remote --heads)" "refs/heads/native/feature"
branch_remote="$(git -C "$clone" config --get branch.native/feature.remote)"
assert_contains "$branch_remote" "origin"

(cd "$clone" && git tag native-v1 && git push origin native-v1 >/dev/null)
assert_contains "$(run_in "$dir" ls-remote --tags)" "refs/tags/native-v1"

(cd "$clone" && git push origin --delete native/feature >/dev/null)
assert_not_contains "$(run_in "$dir" ls-remote --heads)" "refs/heads/native/feature"
if git -C "$clone" config --get branch.native/feature.remote >/dev/null 2>&1; then
  fail "branch tracking should be removed after native branch delete"
fi

printf 'remote update\n' >> "$dir/README.md"
run_in "$dir" add README.md >/dev/null
run_in "$dir" commit -m "remote update" >/dev/null
run_in "$dir" push >/dev/null
if [[ -n "$(git -C "$clone" status --porcelain)" ]]; then
  git -C "$clone" status --porcelain >&2
  fail "native clone should be clean before pull"
fi
(cd "$clone" && git checkout main >/dev/null && git pull >/dev/null)
assert_contains "$(cat "$clone/README.md")" "remote update"
