#!/usr/bin/env bash
source "$(dirname "$0")/../lib/testlib.sh"
init_output="$(init_bgit_repo aws keytypes)"
dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
broker="$(git -C "$dir" config --get bucketgit.broker)"
repo="$(git -C "$dir" config --get bucketgit.logicalRepo)"

accept_with_key() {
  local label="$1"
  local key_path="$2"
  local out code
  out="$(run_in "$dir" admin invite-user --broker "$broker" --user "$label" --role read "$repo")"
  code="$(printf '%s\n' "$out" | awk '/accept-invite/ {print $NF; exit}')"
  [[ "$code" == bgitinv_* ]] || fail "invite code not found in output: $out"
  (
    eval "$(ssh-agent -s)" >/dev/null
    trap 'ssh-agent -k >/dev/null 2>&1 || true' EXIT
    add_test_key "$key_path"
    export BGIT_SSH_KEY="$(native_path "$key_path")"
    cd "$dir"
    "$BGIT" admin accept-invite "$code" >/dev/null
  )
}

accept_with_key ed25519 "$SUITE_ROOT/sshkeys/developer"
accept_with_key rsa "$SUITE_ROOT/sshkeys/rsa_owner"
accept_with_key ecdsa "$SUITE_ROOT/sshkeys/ecdsa_owner"

out="$(run_in "$dir" admin keys list)"
assert_contains "$out" "ed25519"
assert_contains "$out" "rsa"
assert_contains "$out" "ecdsa"
