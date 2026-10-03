#!/usr/bin/env bash
#
# CLI smoke matrix: drives a live fs server with the real S3 command-line
# clients (aws-cli, mc, s3cmd, rclone), covering a bucket create / object
# round-trip / listing / delete cycle with edge-case object key names, and an
# SSE-C (customer key) round trip over TLS for the clients that support it.
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

TLS_PORT="${FS_SMOKE_TLS_PORT:-18443}"
TLS_ENDPOINT="https://127.0.0.1:${TLS_PORT}"
TLS_PID=""

cleanup() {
  if [[ "${KEEP}" == "1" ]]; then
    log "leaving server pid=${SERVER_PID} data=${DATA} running (--keep)"
    return
  fi
  [[ -n "${SERVER_PID}" ]] && kill "${SERVER_PID}" 2>/dev/null || true
  [[ -n "${TLS_PID}" ]] && kill "${TLS_PID}" 2>/dev/null || true
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

# ---- SSE-C over TLS ---------------------------------------------------------

# S3 takes customer keys only over TLS, and minio-go based clients refuse to
# send one otherwise, so SSE-C runs against a second server with a
# self-signed certificate; every client is told not to verify it.
start_tls_server() {
  log "starting TLS server on ${TLS_ENDPOINT}"
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=127.0.0.1" \
    -addext "subjectAltName=IP:127.0.0.1" \
    -keyout "${WORK}/tls.key" -out "${WORK}/tls.crt" >/dev/null 2>&1
  printf 'server:\n  tls:\n    cert_file: %s\n    key_file: %s\n' "${WORK}/tls.crt" "${WORK}/tls.key" > "${WORK}/tls.yaml"
  "${WORK}/fs" s3 --config "${WORK}/tls.yaml" --addr ":${TLS_PORT}" --root "${WORK}/tls-data" --insecure-no-auth \
    > "${WORK}/tls-server.log" 2>&1 &
  TLS_PID=$!

  for _ in $(seq 1 30); do
    if curl -kfsS -o /dev/null "${TLS_ENDPOINT}/health" 2>/dev/null; then
      ok "TLS server healthy (pid=${TLS_PID})"
      return 0
    fi
    sleep 0.5
  done

  cat "${WORK}/tls-server.log"
  echo "TLS server did not become healthy" >&2
  exit 1
}

# The customer key: 32 raw bytes, and base64 for the clients that want it.
SSEC_KEY_FILE="${WORK}/ssec.key"
SSEC_PAYLOAD="${WORK}/ssec.bin"

ssec_fixtures() {
  head -c 32 /dev/urandom > "${SSEC_KEY_FILE}"
  head -c 3000000 /dev/urandom > "${SSEC_PAYLOAD}"
}

ssec_key_b64() { base64 < "${SSEC_KEY_FILE}" | tr -d '\n'; }

smoke_ssec_awscli() {
  command -v aws >/dev/null || return 0
  local aws=(aws --endpoint-url "${TLS_ENDPOINT}" --no-verify-ssl)
  local bucket="ssec-awscli" sse=(--sse-c AES256 --sse-c-key "fileb://${SSEC_KEY_FILE}")

  "${aws[@]}" s3api create-bucket --bucket "${bucket}" >/dev/null 2>&1
  "${aws[@]}" s3 cp "${SSEC_PAYLOAD}" "s3://${bucket}/obj" "${sse[@]}" >/dev/null 2>&1
  "${aws[@]}" s3 cp "s3://${bucket}/obj" "${OUT}/ssec-aws" "${sse[@]}" >/dev/null 2>&1
  cmp -s "${SSEC_PAYLOAD}" "${OUT}/ssec-aws" || { fail "aws-cli: SSE-C round trip mismatch"; return; }

  if "${aws[@]}" s3 cp "s3://${bucket}/obj" "${OUT}/ssec-aws-nokey" >/dev/null 2>&1; then
    fail "aws-cli: SSE-C object read without its key"
    return
  fi

  ok "aws-cli SSE-C round trip; refused without the key"
}

smoke_ssec_mc() {
  local bin
  bin="$(mc_bin)" || return 0
  local mc=("${bin}" --config-dir "${WORK}/mc-tls" --quiet --insecure)
  local bucket="ssec-mc" enc
  enc="tls/${bucket}/=$(ssec_key_b64)"

  "${mc[@]}" alias set tls "${TLS_ENDPOINT}" "${ACCESS_KEY}" "${SECRET_KEY}" --api S3v4 >/dev/null
  "${mc[@]}" mb "tls/${bucket}" >/dev/null
  "${mc[@]}" cp --enc-c "${enc}" "${SSEC_PAYLOAD}" "tls/${bucket}/obj" >/dev/null
  "${mc[@]}" cp --enc-c "${enc}" "tls/${bucket}/obj" "${OUT}/ssec-mc" >/dev/null
  cmp -s "${SSEC_PAYLOAD}" "${OUT}/ssec-mc" || { fail "mc: SSE-C round trip mismatch"; return; }

  if "${mc[@]}" cp "tls/${bucket}/obj" "${OUT}/ssec-mc-nokey" >/dev/null 2>&1; then
    fail "mc: SSE-C object read without its key"
    return
  fi

  ok "mc SSE-C round trip; refused without the key"
}

smoke_ssec_rclone() {
  command -v rclone >/dev/null || return 0
  export RCLONE_CONFIG_SSEC_TYPE=s3
  export RCLONE_CONFIG_SSEC_PROVIDER=Other
  export RCLONE_CONFIG_SSEC_ENV_AUTH=false
  export RCLONE_CONFIG_SSEC_ACCESS_KEY_ID="${ACCESS_KEY}"
  export RCLONE_CONFIG_SSEC_SECRET_ACCESS_KEY="${SECRET_KEY}"
  export RCLONE_CONFIG_SSEC_ENDPOINT="${TLS_ENDPOINT}"
  export RCLONE_CONFIG_SSEC_FORCE_PATH_STYLE=true
  export RCLONE_CONFIG_SSEC_REGION=us-east-1
  export RCLONE_CONFIG_SSEC_SSE_CUSTOMER_ALGORITHM=AES256
  RCLONE_CONFIG_SSEC_SSE_CUSTOMER_KEY_BASE64="$(ssec_key_b64)"
  export RCLONE_CONFIG_SSEC_SSE_CUSTOMER_KEY_BASE64
  : > "${WORK}/rclone.conf"
  local rclone=(rclone --config "${WORK}/rclone.conf" --log-level ERROR --low-level-retries 1 --no-check-certificate)
  local bucket="ssec-rclone"

  "${rclone[@]}" mkdir "ssec:${bucket}" >/dev/null
  "${rclone[@]}" copyto "${SSEC_PAYLOAD}" "ssec:${bucket}/obj" >/dev/null
  "${rclone[@]}" copyto "ssec:${bucket}/obj" "${OUT}/ssec-rclone" >/dev/null
  cmp -s "${SSEC_PAYLOAD}" "${OUT}/ssec-rclone" || { fail "rclone: SSE-C round trip mismatch"; return; }

  ok "rclone SSE-C round trip"
}

# ---- main -------------------------------------------------------------------

make_fixtures
start_server

smoke_awscli
smoke_mc
smoke_s3cmd
smoke_rclone

# s3cmd has no SSE-C support, so it sits this one out.
start_tls_server
ssec_fixtures
smoke_ssec_awscli
smoke_ssec_mc
smoke_ssec_rclone

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
