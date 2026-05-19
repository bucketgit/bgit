#!/usr/bin/env bash
set -euo pipefail
provider="$1"
source "$(dirname "$0")/../testlib.sh"

dir="$(setup_role_repo "$provider" identity)"
commit_file "$dir" README.md "identity" "initial"
run_in "$dir" push -u origin main >/dev/null

developer_fp="$(key_fingerprint developer)"
read_fp="$(key_fingerprint read)"
outsider_fp="$(key_fingerprint outsider)"

out="$(with_agent_key developer bash -c 'cd "$1" && "$2" --identity "$3" whoami --refresh' _ "$dir" "$BGIT" "$developer_fp")"
assert_contains "$out" "role: developer"
assert_contains "$out" "selected identity: $developer_fp"

out="$(
  eval "$(ssh-agent -s)" >/dev/null
  trap 'ssh-agent -k >/dev/null 2>&1 || true' EXIT
  add_test_key "$(key_path outsider)"
  add_test_key "$(key_path developer)"
  cd "$dir"
  "$BGIT" --identity "$developer_fp" whoami --refresh
)"
assert_contains "$out" "role: developer"

out="$(expect_failure_in_as outsider "$dir" whoami --refresh)"
assert_contains "$out" "SSH signature required"
out="$(with_agent_key read bash -c 'cd "$1" && "$2" --identity "$3" whoami --refresh' _ "$dir" "$BGIT" "$read_fp")"
assert_contains "$out" "role: read"

out="$(with_agent_key developer bash -c 'cd "$1" && "$2" whoami --json --refresh' _ "$dir" "$BGIT")"
assert_contains "$out" '"role": "developer"'
assert_contains "$out" '"capabilities"'
out="$(with_agent_key developer bash -c 'cd "$1" && "$2" whoami --json' _ "$dir" "$BGIT")"
assert_contains "$out" '"role": "developer"'

out="$(
  eval "$(ssh-agent -s)" >/dev/null
  trap 'ssh-agent -k >/dev/null 2>&1 || true' EXIT
  add_test_key "$(key_path developer)"
  add_test_key "$(key_path read)"
  cd "$dir"
  "$BGIT" whoami --all
)"
assert_contains "$out" "developer"
assert_contains "$out" "reader"
assert_contains "$out" "warning:"
out="$(
  eval "$(ssh-agent -s)" >/dev/null
  trap 'ssh-agent -k >/dev/null 2>&1 || true' EXIT
  add_test_key "$(key_path developer)"
  cd "$dir"
  "$BGIT" repos mine --json
)"
assert_contains "$out" '"repos"'
assert_contains "$out" '"role": "developer"'

out="$(with_agent_key outsider bash -c 'cd "$1" && "$2" --identity "$3" whoami --refresh' _ "$dir" "$BGIT" "$outsider_fp" 2>&1 || true)"
assert_contains "$out" "SSH signature required"
