#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BGIT="${BGIT:-$ROOT/bgit}"
SUITE_ROOT="$ROOT/testsuite"
RUN_ID="${BGIT_TEST_RUN_ID:-$(date +%Y%m%d%H%M%S)}"
GCP_PROFILE="${BGIT_TEST_GCP_PROFILE:-gcp:local/test}"
AWS_PROFILE="${BGIT_TEST_AWS_PROFILE:-aws:local/test}"
CONFIG_ARGS=()
if [[ -n "${BGIT_TEST_CONFIG:-}" ]]; then
  CONFIG_ARGS=(--config "$BGIT_TEST_CONFIG")
fi

log() { printf '[%s] %s\n' "$(basename "$0")" "$*" >&2; }
fail() { printf '[%s] FAIL: %s\n' "$(basename "$0")" "$*" >&2; exit 1; }

assert_contains() {
  local haystack="$1"
  local needle="$2"
  [[ "$haystack" == *"$needle"* ]] || fail "expected output to contain '$needle'; got: $haystack"
}

assert_not_contains() {
  local haystack="$1"
  local needle="$2"
  [[ "$haystack" != *"$needle"* ]] || fail "expected output not to contain '$needle'; got: $haystack"
}

assert_file_exists() {
  [[ -e "$1" ]] || fail "expected file to exist: $1"
}

assert_file_not_exists() {
  [[ ! -e "$1" ]] || fail "expected file not to exist: $1"
}

expect_success() {
  local out
  if ! out="$("$@" 2>&1)"; then
    printf '%s\n' "$out" >&2
    fail "command failed: $*"
  fi
  printf '%s' "$out"
}

expect_failure() {
  local out
  if out="$("$@" 2>&1)"; then
    printf '%s\n' "$out" >&2
    fail "command unexpectedly succeeded: $*"
  fi
  printf '%s' "$out"
}

provider_profile() {
  case "$1" in
    gcp) printf '%s' "$GCP_PROFILE" ;;
    aws) printf '%s' "$AWS_PROFILE" ;;
    *) fail "unknown provider $1" ;;
  esac
}

provider_dir() {
  printf '%s/%s/repo' "$SUITE_ROOT" "$1"
}

new_repo_name() {
  printf 'bgit-it-%s-%s-%s' "$1" "$RUN_ID" "${2:-repo}"
}

new_workdir() {
  local provider="$1"
  local name="$2"
  local dir
  dir="$(provider_dir "$provider")/$name"
  rm -rf "$dir"
  mkdir -p "$dir"
  printf '%s' "$dir"
}

init_local_git_identity() {
  git -C "$1" config user.name "BucketGit Tests"
  git -C "$1" config user.email "tests@bucketgit.local"
  git -C "$1" config core.autocrlf false
  git -C "$1" config core.eol lf
}

init_bgit_repo() {
  local provider="$1"
  local suffix="$2"
  local profile repo dir
  profile="$(provider_profile "$provider")"
  repo="$(new_repo_name "$provider" "$suffix")"
  dir="$(new_workdir "$provider" "$suffix")"
  expect_success "$BGIT" init --noninteractive --repo "$repo" --profile "$profile" "${CONFIG_ARGS[@]}" "$dir" >/dev/null
  init_local_git_identity "$dir"
  printf '%s\n%s\n' "$dir" "$repo.git"
}

commit_file() {
  local dir="$1"
  local file="$2"
  local body="$3"
  local msg="$4"
  printf '%s\n' "$body" > "$dir/$file"
  (cd "$dir" && "$BGIT" add "$file" && "$BGIT" commit -m "$msg" >/dev/null)
}

run_in() {
  local dir="$1"
  shift
  (cd "$dir" && "$BGIT" "$@")
}

key_path() {
  printf '%s/sshkeys/%s' "$SUITE_ROOT" "$1"
}

key_fingerprint() {
  ssh-keygen -lf "$(key_path "$1.pub")" | awk '{print $2}'
}

native_path() {
  if command -v cygpath >/dev/null 2>&1; then
    cygpath -w "$1"
  else
    printf '%s' "$1"
  fi
}

path_list_separator() {
  if command -v cygpath >/dev/null 2>&1; then
    printf ';'
  else
    printf ':'
  fi
}

add_test_key() {
  local key="$1"
  chmod 600 "$key" >/dev/null 2>&1 || true
  ssh-add "$key" >/dev/null
}

with_agent_key() {
  local key="$1"
  shift
  (
    eval "$(ssh-agent -s)" >/dev/null
    trap 'ssh-agent -k >/dev/null 2>&1 || true' EXIT
    local path
    path="$(key_path "$key")"
    add_test_key "$path"
    export BGIT_SSH_KEY="$(native_path "$path")"
    unset BGIT_SSH_KEYS
    "$@"
  )
}

with_agent_keys() {
  local keys_csv="$1"
  shift
  (
    eval "$(ssh-agent -s)" >/dev/null
    trap 'ssh-agent -k >/dev/null 2>&1 || true' EXIT
    unset BGIT_SSH_KEY
    local sep paths key path
    sep="$(path_list_separator)"
    paths=""
    IFS=',' read -r -a keys <<< "$keys_csv"
    for key in "${keys[@]}"; do
      path="$(key_path "$key")"
      add_test_key "$path"
      if [[ -n "$paths" ]]; then
        paths="${paths}${sep}"
      fi
      paths="${paths}$(native_path "$path")"
    done
    export BGIT_SSH_KEYS="$paths"
    "$@"
  )
}

without_ssh_identity() {
  (
    unset SSH_AUTH_SOCK
    unset BGIT_SSH_KEY
    unset BGIT_SSH_KEYS
    "$@"
  )
}

run_in_as() {
  local key="$1"
  local dir="$2"
  shift 2
  with_agent_key "$key" bash -c 'cd "$1" && shift && "$@"' _ "$dir" "$BGIT" "$@"
}

run_in_no_agent() {
  local dir="$1"
  shift
  (cd "$dir" && without_ssh_identity "$BGIT" "$@")
}

expect_failure_no_agent() {
  local dir="$1"
  shift
  (cd "$dir" && without_ssh_identity expect_failure "$BGIT" "$@")
}

expect_failure_in_as() {
  local key="$1"
  local dir="$2"
  shift 2
  with_agent_key "$key" bash -c '
    dir="$1"
    shift
    cd "$dir"
    if out="$("$@" 2>&1)"; then
      printf "%s\n" "$out" >&2
      exit 99
    fi
    printf "%s" "$out"
  ' _ "$dir" "$BGIT" "$@"
}

add_key_to_repo() {
  local dir="$1"
  local user="$2"
  local role="$3"
  local key="$4"
  run_in "$dir" admin keys add --no-agent --key "$(key_path "$key.pub")" --user "$user" --role "$role" >/dev/null
}

setup_role_repo() {
  local provider="$1"
  local suffix="$2"
  local init_output dir
  init_output="$(init_bgit_repo "$provider" "$suffix")"
  dir="$(printf "%s\n" "$init_output" | sed -n "1p")"
  add_key_to_repo "$dir" admin admin admin
  add_key_to_repo "$dir" maintainer maintainer maintainer
  add_key_to_repo "$dir" developer developer developer
  add_key_to_repo "$dir" triage triage triage
  add_key_to_repo "$dir" reader read read
  printf '%s\n' "$dir"
}
