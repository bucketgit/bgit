#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BGIT="${BGIT:-$ROOT/bgit}"
RUN_ID="${BGIT_TEST_RUN_ID:-$(date +%Y%m%d%H%M%S)}"
WORK_ROOT="${BGIT_REAL_WORK_ROOT:-${TMPDIR:-/tmp}/bgit-real-local-broker-${RUN_ID}}"
BGIT_HOME_DIR="${BGIT_REAL_HOME:-$WORK_ROOT/home}"
AWS_PROFILE="${BGIT_REAL_AWS_PROFILE:-charlieroot}"
AWS_REGION="${BGIT_REAL_AWS_REGION:-eu-west-1}"
GCP_PROFILE="${BGIT_REAL_GCP_PROFILE:-riedel}"
GCP_REGION="${BGIT_REAL_GCP_REGION:-europe-west1}"

mkdir -p "$WORK_ROOT" "$BGIT_HOME_DIR"
export BGIT_HOME="$BGIT_HOME_DIR"
AWS_BUCKETS=()
GCP_BUCKETS=()

log() { printf '[local-broker-cloud] %s\n' "$*" >&2; }
fail() { printf '[local-broker-cloud] FAIL: %s\n' "$*" >&2; exit 1; }

assert_file_contains() {
  local path="$1"
  local needle="$2"
  [[ -f "$path" ]] || fail "missing file $path"
  grep -F "$needle" "$path" >/dev/null || fail "$path does not contain $needle"
}

init_identity() {
  git -C "$1" config user.name "BucketGit Real Smoke"
  git -C "$1" config user.email "smoke@bucketgit.local"
}

cleanup_aws_bucket() {
  local bucket="$1"
  [[ -n "$bucket" ]] || return 0
  aws --profile "$AWS_PROFILE" --region "$AWS_REGION" s3 rm "s3://$bucket" --recursive >/dev/null 2>&1 || true
  aws --profile "$AWS_PROFILE" --region "$AWS_REGION" s3api delete-bucket --bucket "$bucket" >/dev/null 2>&1 || true
}

cleanup_gcp_bucket() {
  local bucket="$1"
  [[ -n "$bucket" ]] || return 0
  gcloud --configuration "$GCP_PROFILE" storage rm -r "gs://$bucket" --quiet >/dev/null 2>&1 || true
}

cleanup_all() {
  local bucket
  for bucket in "${AWS_BUCKETS[@]}"; do cleanup_aws_bucket "$bucket"; done
  for bucket in "${GCP_BUCKETS[@]}"; do cleanup_gcp_bucket "$bucket"; done
}
trap cleanup_all EXIT

exercise_repo() {
  local scheme="$1"
  local profile="$2"
  local region="$3"
  local repo="bgit-real-${scheme}-${RUN_ID}.git"
  local first="$WORK_ROOT/${scheme}-first"
  local second="$WORK_ROOT/${scheme}-second"
  local uri="${scheme}://${repo}"

  log "cloning ${uri} via local broker using profile ${profile} region ${region}"
  "$BGIT" --profile "$profile" --region "$region" clone "$uri" "$first" >/dev/null
  init_identity "$first"
  local bucket
  bucket="$(git -C "$first" config --get bucketgit.bucket || true)"
  case "$scheme" in
    s3) AWS_BUCKETS+=("$bucket") ;;
    gs) GCP_BUCKETS+=("$bucket") ;;
  esac

  printf '%s initial\n' "$scheme" > "$first/README.md"
  (cd "$first" && "$BGIT" add README.md && "$BGIT" commit -m "initial" >/dev/null && "$BGIT" push -u origin main >/dev/null)

  "$BGIT" --profile "$profile" --region "$region" clone "$uri" "$second" >/dev/null
  init_identity "$second"
  assert_file_contains "$second/README.md" "$scheme initial"

  printf '%s second\n' "$scheme" >> "$second/README.md"
  (cd "$second" && "$BGIT" add README.md && "$BGIT" commit -m "second" >/dev/null && "$BGIT" push >/dev/null)
  out="$(cd "$first" && "$BGIT" pull)"
  [[ "$out" == *"Fast-forward"* || "$out" == *"README.md"* ]] || fail "pull output did not show update: $out"
  assert_file_contains "$first/README.md" "$scheme second"

  (cd "$first" && "$BGIT" checkout -b "smoke/${scheme}" >/dev/null)
  printf '%s branch\n' "$scheme" > "$first/branch.txt"
  (cd "$first" && "$BGIT" add branch.txt && "$BGIT" commit -m "branch" >/dev/null && "$BGIT" push -u origin "smoke/${scheme}" >/dev/null)
  out="$(cd "$second" && "$BGIT" ls-remote)"
  [[ "$out" == *"refs/heads/smoke/${scheme}"* ]] || fail "remote branch not visible: $out"
  log "completed ${uri}"
}

target="${1:-all}"
case "$target" in
  all)
    exercise_repo s3 "$AWS_PROFILE" "$AWS_REGION"
    exercise_repo gs "$GCP_PROFILE" "$GCP_REGION"
    ;;
  s3|aws)
    exercise_repo s3 "$AWS_PROFILE" "$AWS_REGION"
    ;;
  gs|gcp)
    exercise_repo gs "$GCP_PROFILE" "$GCP_REGION"
    ;;
  *)
    fail "usage: $0 [all|s3|gs]"
    ;;
esac

log "passed"
