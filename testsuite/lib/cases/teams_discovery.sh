#!/usr/bin/env bash
source "$(dirname "$0")/../testlib.sh"

provider="$1"
scoped_output="$(init_bgit_repo_no_owner_access "$provider" scoped-owner)"
scoped_dir="$(printf "%s\n" "$scoped_output" | sed -n "1p")"
out="$(run_in_as owner "$scoped_dir" whoami --refresh)"
assert_contains "$out" "role: none"
assert_contains "$out" "admin_keys"
run_in "$scoped_dir" admin keys add --no-agent --key "$(key_path owner.pub)" --user owner --role read >/dev/null
out="$(expect_failure_in_as owner "$scoped_dir" admin keys add --no-agent --key "$(key_path owner.pub)" --user other-owner --role read)"
assert_contains "$out" "SSH key already belongs to user owner"
out="$(run_in_as owner "$scoped_dir" whoami --refresh)"
assert_contains "$out" "role: read"
commit_file "$scoped_dir" README.md "scoped owner" "scoped owner"
out="$(expect_failure_in_as owner "$scoped_dir" push)"
assert_contains "$out" "write SSH signature required"

init_output="$(init_bgit_repo "$provider" teams)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"

out="$(run_in "$dir" admin teams list)"
assert_contains "$out" "t_core"
assert_contains "$out" "core"

out="$(run_in "$dir" admin broker-users upsert teamdev --role user --key "$(key_path developer.pub)")"
assert_contains "$out" "upserted broker user teamdev as user"
out="$(expect_failure_in_as owner "$dir" admin broker-users upsert duplicate-dev --role user --key "$(key_path developer.pub)")"
assert_contains "$out" "SSH key already belongs to broker user teamdev"
out="$(run_in "$dir" admin broker-users upsert brokeradmin --role admin --key "$(key_path admin.pub)")"
assert_contains "$out" "upserted broker user brokeradmin as admin"
out="$(run_in_as admin "$dir" admin broker-users upsert delegated-admin --role admin --key "$(key_path triage.pub)")"
assert_contains "$out" "upserted broker user delegated-admin as admin"
out="$(run_in_as triage "$dir" admin teams create delegated-active)"
assert_contains "$out" "created team delegated-active"
out="$(run_in "$dir" admin broker-users upsert delegated-admin --role admin --suspended true)"
assert_contains "$out" "upserted broker user delegated-admin as admin"
out="$(run_in "$dir" admin broker-users list)"
assert_contains "$out" "delegated-admin"
assert_contains "$out" "suspended"
out="$(expect_failure_in_as triage "$dir" admin teams create delegated-suspended)"
assert_contains "$out" "broker admin SSH signature required"
out="$(run_in "$dir" admin broker-users upsert delegated-admin --role admin --suspended false)"
assert_contains "$out" "upserted broker user delegated-admin as admin"
out="$(run_in_as triage "$dir" admin teams create delegated-unsuspended)"
assert_contains "$out" "created team delegated-unsuspended"
out="$(expect_failure_in_as admin "$dir" admin broker-users upsert forbidden-owner --role owner --key "$(key_path read.pub)")"
assert_contains "$out" "invalid broker role"
out="$(cd "$dir" && expect_failure "$BGIT" admin broker-users upsert forbidden-owner --role owner --key "$(key_path read.pub)")"
assert_contains "$out" "invalid broker role"
out="$(cd "$dir" && expect_failure "$BGIT" admin broker-users upsert invalid-role --role madeup --key "$(key_path read.pub)")"
assert_contains "$out" "invalid broker role"
out="$(expect_failure_in_as owner "$dir" admin broker-users upsert owner --role user)"
assert_contains "$out" "broker owner cannot be reassigned or suspended"
out="$(expect_failure_in_as owner "$dir" admin broker-users upsert owner --role owner)"
assert_contains "$out" "invalid broker role"
out="$(expect_failure_in_as owner "$dir" admin broker-users delete owner)"
assert_contains "$out" "broker owner cannot be deleted"

out="$(run_in "$dir" admin teams create platform)"
assert_contains "$out" "created team platform"
team_id="$(printf '%s\n' "$out" | sed -n 's/.*(\(t_[^)]*\)).*/\1/p')"
[[ -n "$team_id" ]] || fail "team id not found in output: $out"
out="$(expect_failure_in_as developer "$dir" admin teams delete "$team_id")"
assert_contains "$out" "broker admin SSH signature required"
out="$(expect_failure "$BGIT" --profile "$(provider_profile "$provider")" admin teams delete t_core)"
assert_contains "$out" "core team cannot be deleted"

out="$(run_in_as admin "$dir" admin teams create admin-created)"
assert_contains "$out" "created team admin-created"

out="$(expect_failure_in_as developer "$dir" admin broker-users upsert denied --role user --key "$(key_path read.pub)")"
assert_contains "$out" "broker admin SSH signature required"
out="$(expect_failure_in_as developer "$dir" admin teams create denied)"
assert_contains "$out" "broker admin SSH signature required"

out="$(run_in "$dir" admin broker-users upsert tempdel --role user)"
assert_contains "$out" "upserted broker user tempdel as user"
run_in "$dir" admin teams member add "$team_id" tempdel --role developer >/dev/null
run_in "$dir" admin keys add --no-agent --key "$(key_path read.pub)" --user tempdel --role developer >/dev/null
out="$(run_in "$dir" admin broker-users delete tempdel)"
assert_contains "$out" "deleted broker user tempdel"
out="$(run_in "$dir" admin broker-users list)"
assert_not_contains "$out" "tempdel"
out="$(run_in "$dir" admin teams list)"
assert_not_contains "$out" "tempdel"
out="$(run_in "$dir" admin keys list)"
assert_not_contains "$out" "tempdel"
out="$(expect_failure_in_as read "$dir" whoami --refresh)"
assert_contains "$out" "SSH signature required"

run_in "$dir" admin teams member add "$team_id" teamdev --role read >/dev/null
run_in_as admin "$dir" admin teams member add "$team_id" brokeradmin --role developer >/dev/null
out="$(run_in "$dir" admin teams list)"
assert_contains "$out" "teamdev:read"
assert_contains "$out" "brokeradmin:developer"
run_in "$dir" admin teams repo add "$team_id" read >/dev/null
run_in "$dir" admin teams repo add "$team_id" read >/dev/null
out="$(run_in "$dir" admin teams repo list)"
assert_contains "$out" "$team_id"
grant_count="$(printf '%s\n' "$out" | grep -c "^$team_id[[:space:]]")"
[[ "$grant_count" == "1" ]] || fail "expected idempotent team repo grant; got: $out"
out="$(expect_failure_in_as developer "$dir" admin teams repo list)"
assert_contains "$out" "admin SSH signature required"
out="$(expect_failure "$BGIT" --profile "$(provider_profile "$provider")" admin teams repo add "$team_id" read)"
assert_contains "$out" "repo is required"
out="$(expect_failure_in_as owner "$dir" admin teams repo add missing-team read)"
assert_contains "$out" "team not found"
out="$(run_in_as owner "$dir" admin teams repo remove missing-team)"
assert_contains "$out" "detached team missing-team"
original_logical="$(git -C "$dir" config --get bucketgit.logicalRepo)"
git -C "$dir" config bucketgit.logicalRepo "$(new_repo_name "$provider" missing-team-grant).git"
out="$(expect_failure_in_as owner "$dir" admin teams repo list)"
assert_contains "$out" "repository not found"
out="$(expect_failure_in_as owner "$dir" admin teams repo add "$team_id" read)"
assert_contains "$out" "repository not found"
out="$(expect_failure_in_as owner "$dir" admin repo info)"
assert_contains "$out" "repository not found"
git -C "$dir" config bucketgit.logicalRepo "$original_logical"

out="$(run_in "$dir" admin teams list)"
assert_contains "$out" "$team_id"
assert_contains "$out" "platform"

commit_file "$dir" README.md "owner seed" "owner seed"
run_in "$dir" push >/dev/null

with_agent_key developer bash -c '
  set -euo pipefail
  dir="$1"
  cd "$dir"
  printf "team read should not write\n" > TEAM.md
  "$BGIT" add TEAM.md
  "$BGIT" commit -m "team read should not write" >/dev/null
  if out="$("$BGIT" push 2>&1)"; then
    printf "%s\n" "$out" >&2
    exit 99
  fi
  [[ "$out" == *"write SSH signature required"* ]]
' _ "$dir"

run_in "$dir" admin teams repo add "$team_id" developer >/dev/null
with_agent_key developer bash -c '
  set -euo pipefail
  dir="$1"
  cd "$dir"
  printf "team member read should still not write\n" >> TEAM.md
  "$BGIT" add TEAM.md
  "$BGIT" commit -m "team member read should still not write" >/dev/null
  if out="$("$BGIT" push 2>&1)"; then
    printf "%s\n" "$out" >&2
    exit 99
  fi
  [[ "$out" == *"write SSH signature required"* ]]
' _ "$dir"

run_in "$dir" admin teams member add "$team_id" teamdev --role developer >/dev/null
out="$(run_in "$dir" admin teams list)"
assert_contains "$out" "teamdev:developer"
run_in "$dir" admin teams repo add "$team_id" developer >/dev/null

with_agent_key developer bash -c '
  set -euo pipefail
  dir="$1"
  cd "$dir"
  printf "team write\n" >> TEAM.md
  "$BGIT" add TEAM.md
  "$BGIT" commit -m "team write" >/dev/null
  "$BGIT" push >/dev/null
' _ "$dir"

out="$(run_in_as developer "$dir" whoami)"
assert_contains "$out" "teamdev"
assert_contains "$out" "developer"

out="$(expect_failure_in_as developer "$dir" admin teams repo add "$team_id" admin)"
assert_contains "$out" "admin SSH signature required"

run_in "$dir" admin teams repo remove "$team_id" >/dev/null
out="$(expect_failure_in_as developer "$dir" whoami --refresh)"
assert_contains "$out" "SSH signature required"

run_in "$dir" admin teams repo add "$team_id" developer >/dev/null
run_in "$dir" admin teams member remove "$team_id" teamdev >/dev/null
out="$(expect_failure_in_as developer "$dir" whoami --refresh)"
assert_contains "$out" "SSH signature required"

run_in "$dir" admin teams member add "$team_id" teamdev --role developer >/dev/null
out="$(run_in_as developer "$dir" whoami --refresh)"
assert_contains "$out" "teamdev"
assert_contains "$out" "role: developer"

out="$(run_in "$dir" admin teams create qa)"
assert_contains "$out" "created team qa"
second_team_id="$(printf '%s\n' "$out" | sed -n 's/.*(\(t_[^)]*\)).*/\1/p')"
[[ -n "$second_team_id" ]] || fail "second team id not found in output: $out"
run_in "$dir" admin teams member add "$second_team_id" teamdev --role admin >/dev/null
run_in "$dir" admin teams repo add "$second_team_id" admin >/dev/null
out="$(run_in_as developer "$dir" whoami --refresh)"
assert_contains "$out" "role: admin"
run_in "$dir" admin teams repo remove "$second_team_id" >/dev/null
out="$(run_in_as developer "$dir" whoami --refresh)"
assert_contains "$out" "role: developer"
run_in "$dir" admin teams repo add "$second_team_id" admin >/dev/null
run_in "$dir" admin teams member remove "$second_team_id" teamdev >/dev/null
out="$(run_in_as developer "$dir" whoami --refresh)"
assert_contains "$out" "role: developer"
run_in "$dir" admin teams delete "$second_team_id" >/dev/null
out="$(run_in "$dir" admin teams list)"
assert_not_contains "$out" "$second_team_id"

run_in "$dir" admin teams member add "$team_id" teamdev --role owner >/dev/null
run_in "$dir" admin teams repo add "$team_id" owner >/dev/null
out="$(run_in_as developer "$dir" whoami --refresh)"
assert_contains "$out" "role: owner"
assert_contains "$out" "owner_transfer"
