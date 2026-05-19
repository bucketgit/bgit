#!/usr/bin/env bash
set -euo pipefail
provider="$1"
source "$(dirname "$0")/../testlib.sh"

dir="$(setup_role_repo "$provider" protection)"
commit_file "$dir" README.md "base" "initial"
run_in "$dir" push -u origin main >/dev/null

run_in "$dir" admin protect add main >/dev/null
out="$(run_in "$dir" admin protect list)"
assert_contains "$out" "refs/heads/main"
assert_contains "$out" "pr-required"

printf 'blocked\n' >> "$dir/README.md"
run_in "$dir" add README.md >/dev/null
run_in "$dir" commit -m "blocked direct push" >/dev/null
out="$(cd "$dir" && expect_failure "$BGIT" push)"
assert_contains "$out" "protected branch refs/heads/main requires a pull request"

run_in "$dir" checkout -b feature/protected >/dev/null
printf 'via pr\n' > "$dir/feature.txt"
run_in "$dir" add feature.txt >/dev/null
run_in "$dir" commit -m "feature protected" >/dev/null
run_in "$dir" push -u origin feature/protected >/dev/null
out="$(run_in "$dir" pr create --title "Protected merge" --source feature/protected --target main)"
assert_contains "$out" "created PR #"
id="$(run_in "$dir" pr list | sed -n 's/^#\([0-9][0-9]*\).*/\1/p' | head -1)"
out="$(run_in_as maintainer "$dir" pr merge "$id")"
assert_contains "$out" "merged PR #$id"

run_in "$dir" admin protect remove main >/dev/null
out="$(run_in "$dir" admin protect list)"
assert_not_contains "$out" "refs/heads/main"

run_in "$dir" checkout main >/dev/null
run_in "$dir" pull >/dev/null
run_in "$dir" admin protect add main --allow-owner-admin-override >/dev/null
printf 'owner override\n' >> "$dir/README.md"
run_in "$dir" add README.md >/dev/null
run_in "$dir" commit -m "owner override" >/dev/null
out="$(run_in "$dir" push --force)"
assert_contains "$out" "main -> main"
run_in "$dir" admin protect remove main >/dev/null

run_in "$dir" checkout main >/dev/null
printf 'readonly\n' >> "$dir/README.md"
run_in "$dir" add README.md >/dev/null
run_in "$dir" commit -m "readonly check" >/dev/null
run_in "$dir" admin repo readonly on >/dev/null
out="$(expect_failure_in_as developer "$dir" push)"
assert_contains "$out" "repository is read-only"
run_in "$dir" admin repo readonly off >/dev/null

run_in "$dir" checkout -b delete/me >/dev/null
printf 'delete\n' > "$dir/delete.txt"
run_in "$dir" add delete.txt >/dev/null
run_in "$dir" commit -m "delete branch" >/dev/null
run_in "$dir" push -u origin delete/me >/dev/null
out="$(run_in "$dir" push --delete delete/me)"
assert_contains "$out" "deleted"
out="$(run_in "$dir" ls-remote --heads)"
assert_not_contains "$out" "refs/heads/delete/me"
