#!/usr/bin/env bash
#
# CLI smoke matrix: drives a live fs server with the real S3 command-line
# clients (aws-cli, mc, s3cmd, rclone), covering a bucket create / object
# round-trip / listing / delete cycle with edge-case object key names.
#
# A missing client is skipped (warned, not failed) so the script is usable on a
# workstation; CI installs all four, so every client is exercised there. Any
# client that IS present must pass every step or the script exits non-zero.
#
# Usage: scripts/cli-smoke.sh [--keep]
#   --keep   leave the server running and the data dir in place on exit.

set -euo pipefail

PORT="${FS_SMOKE_PORT:-18080}"
ENDPOINT="http://127.0.0.1:${PORT}"
ACCESS_KEY="smoke"
SECRET_KEY="smokesecret"
KEEP=0
[[ "${1:-}" == "--keep" ]] && KEEP=1

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
DATA="${WORK}/data"
FILES="${WORK}/files"
OUT="${WORK}/out"
mkdir -p "${DATA}" "${FILES}" "${OUT}"

SERVER_PID=""
FAILED=0
declare -a RAN=() SKIPPED=()

log()  { printf '\033[1;34m[smoke]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ok\033[0m   %s\n' "$*"; }
warn() { printf '\033[1;33m  skip\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m  FAIL\033[0m %s\n' "$*"; FAILED=1; }

cleanup() {
  if [[ "${KEEP}" == "1" ]]; then
    log "leaving server pid=${SERVER_PID} data=${DATA} running (--keep)"
    return
  fi
  [[ -n "${SERVER_PID}" ]] && kill "${SERVER_PID}" 2>/dev/null || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

# Edge-case object keys exercised by every client. Kept deliberately gnarly:
# spaces, unicode, plus/percent, and nested "directories".
KEYS=(
  "plain.txt"
  "with space.txt"
  "nested/dir/deep.txt"
  "unicode-café-日本.txt"
  "plus+sign.txt"
  "percent%20literal.txt"
  "dots.and-dashes_v1.2.3.txt"
)

make_fixtures() {
  local i=0
  for key in "${KEYS[@]}"; do
    local f="${FILES}/f${i}"
    printf 'content for key: %s\n' "${key}" > "${f}"
    i=$((i + 1))
  done
}

# ---- server -----------------------------------------------------------------

start_server() {
  log "building fs binary"
  (cd "${ROOT}" && go build -o "${WORK}/fs" ./cmd/fs)

  log "starting server on ${ENDPOINT}"
  # The smoke matrix drives the clients with arbitrary credentials the server
  # ignores, so run without authentication (auth is ON by default).
  "${WORK}/fs" s3 --addr ":${PORT}" --root "${DATA}" --insecure-no-auth > "${WORK}/server.log" 2>&1 &
  SERVER_PID=$!

  for _ in $(seq 1 30); do
    if curl -fsS -o /dev/null "${ENDPOINT}/health" 2>/dev/null; then
      ok "server healthy (pid=${SERVER_PID})"
      return 0
    fi
    sleep 0.5
  done

  cat "${WORK}/server.log"
  echo "server did not become healthy" >&2
  exit 1
}

# verify_download compares a downloaded file against its fixture.
verify_download() {
  local idx="$1" got="$2" client="$3" key="$4"
  if cmp -s "${FILES}/f${idx}" "${got}"; then
    return 0
  fi
  fail "${client}: content mismatch for key '${key}'"
  return 1
}

# ---- aws-cli ----------------------------------------------------------------

smoke_awscli() {
  command -v aws >/dev/null || { SKIPPED+=("aws-cli"); warn "aws-cli not installed"; return; }
  RAN+=("aws-cli")
  log "aws-cli"

  local bucket="smoke-awscli"
  export AWS_ACCESS_KEY_ID="${ACCESS_KEY}" AWS_SECRET_ACCESS_KEY="${SECRET_KEY}"
  export AWS_EC2_METADATA_DISABLED=true AWS_DEFAULT_REGION=us-east-1
  local aws=(aws --endpoint-url "${ENDPOINT}")

  "${aws[@]}" s3api create-bucket --bucket "${bucket}" >/dev/null

  local i=0
  for key in "${KEYS[@]}"; do
    "${aws[@]}" s3api put-object --bucket "${bucket}" --key "${key}" --body "${FILES}/f${i}" >/dev/null
    "${aws[@]}" s3api get-object --bucket "${bucket}" --key "${key}" "${OUT}/aws-${i}" >/dev/null
    verify_download "${i}" "${OUT}/aws-${i}" aws-cli "${key}"
    i=$((i + 1))
  done

  local count
  count=$("${aws[@]}" s3api list-objects-v2 --bucket "${bucket}" --query 'length(Contents)' --output text)
  [[ "${count}" == "${#KEYS[@]}" ]] || fail "aws-cli: listed ${count}, want ${#KEYS[@]}"

  # High-level cp path (multipart-capable) plus recursive list.
  "${aws[@]}" s3 cp "${FILES}/f0" "s3://${bucket}/hl/copy.txt" >/dev/null
  "${aws[@]}" s3 rm "s3://${bucket}/hl/copy.txt" >/dev/null

  i=0
  for key in "${KEYS[@]}"; do
    "${aws[@]}" s3api delete-object --bucket "${bucket}" --key "${key}" >/dev/null
    i=$((i + 1))
  done
  "${aws[@]}" s3api delete-bucket --bucket "${bucket}" >/dev/null
  ok "aws-cli round-trip over ${#KEYS[@]} edge-case keys"
}

# ---- mc (MinIO client) ------------------------------------------------------

# mc_bin resolves the real MinIO client, which ships as "mcli" on systems where
# "mc" is GNU Midnight Commander. Prints the binary name, or nothing if absent.
mc_bin() {
  local b
  for b in mcli mc; do
    if command -v "${b}" >/dev/null && "${b}" --version 2>&1 | grep -qiE 'minio|mc version'; then
      echo "${b}"
      return 0
    fi
  done
  return 1
}

smoke_mc() {
  local bin
  bin="$(mc_bin)" || { SKIPPED+=("mc"); warn "MinIO client (mc/mcli) not installed"; return; }
  RAN+=("mc")
  log "mc (${bin})"

  local bucket="smoke-mc"
  local cfg="${WORK}/mc"
  local mc=("${bin}" --config-dir "${cfg}" --quiet)
  "${mc[@]}" alias set smoke "${ENDPOINT}" "${ACCESS_KEY}" "${SECRET_KEY}" --api S3v4 >/dev/null
  "${mc[@]}" mb "smoke/${bucket}" >/dev/null

  local i=0
  for key in "${KEYS[@]}"; do
    "${mc[@]}" cp "${FILES}/f${i}" "smoke/${bucket}/${key}" >/dev/null
    "${mc[@]}" cp "smoke/${bucket}/${key}" "${OUT}/mc-${i}" >/dev/null
    verify_download "${i}" "${OUT}/mc-${i}" mc "${key}"
    i=$((i + 1))
  done

  local count
  count=$("${mc[@]}" ls --recursive "smoke/${bucket}" | wc -l | tr -d ' ')
  [[ "${count}" == "${#KEYS[@]}" ]] || fail "mc: listed ${count}, want ${#KEYS[@]}"

  "${mc[@]}" rb --force "smoke/${bucket}" >/dev/null
  ok "mc round-trip over ${#KEYS[@]} edge-case keys"
}

# ---- s3cmd ------------------------------------------------------------------

smoke_s3cmd() {
  command -v s3cmd >/dev/null || { SKIPPED+=("s3cmd"); warn "s3cmd not installed"; return; }
  RAN+=("s3cmd")
  log "s3cmd"

  local bucket="smoke-s3cmd"
  local host="127.0.0.1:${PORT}"
  local cfg="${WORK}/s3cmd.cfg"
  cat > "${cfg}" <<EOF
[default]
access_key = ${ACCESS_KEY}
secret_key = ${SECRET_KEY}
host_base = ${host}
host_bucket = ${host}
use_https = False
signature_v2 = False
EOF
  local s3cmd=(s3cmd -c "${cfg}")

  "${s3cmd[@]}" mb "s3://${bucket}" >/dev/null

  local i=0
  for key in "${KEYS[@]}"; do
    "${s3cmd[@]}" put "${FILES}/f${i}" "s3://${bucket}/${key}" >/dev/null
    "${s3cmd[@]}" get --force "s3://${bucket}/${key}" "${OUT}/s3cmd-${i}" >/dev/null
    verify_download "${i}" "${OUT}/s3cmd-${i}" s3cmd "${key}"
    i=$((i + 1))
  done

  local count
  count=$("${s3cmd[@]}" ls --recursive "s3://${bucket}" | wc -l | tr -d ' ')
  [[ "${count}" == "${#KEYS[@]}" ]] || fail "s3cmd: listed ${count}, want ${#KEYS[@]}"

  "${s3cmd[@]}" rb --force --recursive "s3://${bucket}" >/dev/null
  ok "s3cmd round-trip over ${#KEYS[@]} edge-case keys"
}

# ---- rclone -----------------------------------------------------------------

smoke_rclone() {
  command -v rclone >/dev/null || { SKIPPED+=("rclone"); warn "rclone not installed"; return; }
  RAN+=("rclone")
  log "rclone"

  local bucket="smoke-rclone"
  # Configure the remote entirely through env vars (no config file).
  export RCLONE_CONFIG_SMOKE_TYPE=s3
  export RCLONE_CONFIG_SMOKE_PROVIDER=Other
  export RCLONE_CONFIG_SMOKE_ENV_AUTH=false
  export RCLONE_CONFIG_SMOKE_ACCESS_KEY_ID="${ACCESS_KEY}"
  export RCLONE_CONFIG_SMOKE_SECRET_ACCESS_KEY="${SECRET_KEY}"
  export RCLONE_CONFIG_SMOKE_ENDPOINT="${ENDPOINT}"
  export RCLONE_CONFIG_SMOKE_FORCE_PATH_STYLE=true
  export RCLONE_CONFIG_SMOKE_REGION=us-east-1
  # Empty config file silences the "config not found" NOTICE; the remote is
  # defined entirely through the env vars above.
  : > "${WORK}/rclone.conf"
  local rclone=(rclone --config "${WORK}/rclone.conf" --log-level ERROR --low-level-retries 1)

  "${rclone[@]}" mkdir "smoke:${bucket}" >/dev/null

  local i=0
  for key in "${KEYS[@]}"; do
    "${rclone[@]}" copyto "${FILES}/f${i}" "smoke:${bucket}/${key}" >/dev/null
    "${rclone[@]}" copyto "smoke:${bucket}/${key}" "${OUT}/rclone-${i}" >/dev/null
    verify_download "${i}" "${OUT}/rclone-${i}" rclone "${key}"
    i=$((i + 1))
  done

  # --files-only: rclone otherwise also emits synthetic dir markers (nested/).
  local count
  count=$("${rclone[@]}" lsf --recursive --files-only "smoke:${bucket}" | wc -l | tr -d ' ')
  [[ "${count}" == "${#KEYS[@]}" ]] || fail "rclone: listed ${count}, want ${#KEYS[@]}"

  "${rclone[@]}" purge "smoke:${bucket}" >/dev/null
  ok "rclone round-trip over ${#KEYS[@]} edge-case keys"
}

# ---- main -------------------------------------------------------------------

make_fixtures
start_server

smoke_awscli
smoke_mc
smoke_s3cmd
smoke_rclone

log "clients exercised: ${RAN[*]:-none}"
[[ ${#SKIPPED[@]} -gt 0 ]] && log "clients skipped:   ${SKIPPED[*]}"

if [[ ${#RAN[@]} -eq 0 ]]; then
  echo "no S3 clients available to smoke-test" >&2
  exit 1
fi

if [[ "${FAILED}" == "1" ]]; then
  log "RESULT: FAIL"
  exit 1
fi

log "RESULT: PASS"
