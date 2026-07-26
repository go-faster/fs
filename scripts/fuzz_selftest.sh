#!/usr/bin/env bash
# Checks that scripts/fuzz.sh fails on real fuzz findings and only tolerates a
# search stopped by its own deadline.
#
# fuzz.sh is the script that decides whether the fuzz job is red, and it now
# retries one specific failure instead of reporting it. That tolerance is worth
# exactly as much as the evidence it demands, so each case below drives the real
# script with a fake `go` on PATH that replays recorded output.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fuzz="${here}/fuzz.sh"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# stub_go installs a fake `go` that prints the given file and exits with the
# given status. When two are given, the first call uses the first pair and every
# later call uses the second — which is how a retry is exercised.
stub_go() {
  local first_out="$1" first_status="$2" rest_out="${3:-$1}" rest_status="${4:-$2}"
  local dir="${work}/bin"

  mkdir -p "$dir"

  cat >"${dir}/go" <<EOF
#!/usr/bin/env bash
# Only fuzz runs are stubbed; anything else would mean the script changed shape.
case " \$* " in
  *" -fuzz "*) ;;
  *) echo "unexpected go invocation: \$*" >&2; exit 99 ;;
esac

count_file="${work}/calls"
count=\$(cat "\$count_file" 2>/dev/null || echo 0)
echo \$((count + 1)) >"\$count_file"

if [ "\$count" -eq 0 ]; then
  cat "${first_out}"
  exit ${first_status}
fi

cat "${rest_out}"
exit ${rest_status}
EOF

  chmod +x "${dir}/go"
  rm -f "${work}/calls"
}

# run_fuzz drives one target through the real script with the stub in place.
run_fuzz() {
  (
    cd "${here}/.." || exit 1
    PATH="${work}/bin:$PATH" FUZZTIME=1s "$fuzz" >"${work}/log" 2>&1
  )
}

# Recorded output of a run whose budget expired mid-execution: the exact shape
# that turned a completed search red on 2026-07-26.
cat >"${work}/deadline.txt" <<'EOF'
fuzz: elapsed: 0s, gathering baseline coverage: 0/56 completed
fuzz: elapsed: 0s, gathering baseline coverage: 56/56 completed, now fuzzing with 4 workers
fuzz: elapsed: 3s, execs: 14745 (4914/sec), new interesting: 24 (total: 80)
--- FAIL: FuzzDecodeDeleteObjects (3.01s)
    context deadline exceeded
FAIL
exit status 1
FAIL	github.com/go-faster/fs/internal/core/handler	3.012s
EOF

cat >"${work}/pass.txt" <<'EOF'
fuzz: elapsed: 0s, gathering baseline coverage: 56/56 completed, now fuzzing with 4 workers
fuzz: elapsed: 2s, execs: 9001 (4500/sec), new interesting: 3 (total: 59)
PASS
ok  	github.com/go-faster/fs/internal/core/handler	2.011s
EOF

# A crasher, constructed to also carry "context deadline exceeded" so that
# tolerance cannot rest on that string alone. Whether a real finding ever prints
# it alongside is not the point: if one did, a naive match would swallow it.
cat >"${work}/crasher.txt" <<'EOF'
fuzz: elapsed: 1s, execs: 512 (512/sec), new interesting: 1 (total: 57)
--- FAIL: FuzzDecodeDeleteObjects (1.02s)
    --- FAIL: FuzzDecodeDeleteObjects (0.00s)
        decode_test.go:41: decoder accepted malformed body
    context deadline exceeded

    Failing input written to testdata/fuzz/FuzzDecodeDeleteObjects/a1b2c3
    To re-run:
    go test -run=FuzzDecodeDeleteObjects/a1b2c3
FAIL
exit status 1
EOF

cat >"${work}/panic.txt" <<'EOF'
fuzz: elapsed: 1s, execs: 88 (88/sec), new interesting: 0 (total: 12)
panic: runtime error: index out of range [3] with length 2
    context deadline exceeded
FAIL
exit status 2
EOF

cat >"${work}/hang.txt" <<'EOF'
fuzz: elapsed: 30s, execs: 4 (0/sec), new interesting: 0 (total: 4)
panic: test timed out after 10m0s
    context deadline exceeded
FAIL
exit status 2
EOF

fails=0

check() {
  local name="$1" want="$2" got="$3"

  if [ "$want" = "$got" ]; then
    echo "ok   ${name}"

    return
  fi

  echo "FAIL ${name}: expected exit ${want}, got ${got}" >&2
  sed 's/^/     | /' "${work}/log" >&2
  fails=1
}

# A clean run passes, and nothing is retried.
stub_go "${work}/pass.txt" 0
status=0
run_fuzz || status=$?
check "clean run passes" 0 "$status"

# The deadline case: red on the first attempt, green on the retry — reported as
# a retry, not silently.
stub_go "${work}/deadline.txt" 1 "${work}/pass.txt" 0
status=0
run_fuzz || status=$?
check "expired budget is retried" 0 "$status"
grep -q "retrying once" "${work}/log" || { echo "FAIL retry was not announced" >&2; fails=1; }

# Reproducible, so not a flake.
stub_go "${work}/deadline.txt" 1
status=0
run_fuzz || status=$?
check "expired budget twice fails" 1 "$status"

# The three that must never be tolerated, each of which also says "context
# deadline exceeded".
for case in crasher panic hang; do
  stub_go "${work}/${case}.txt" 1
  status=0
  run_fuzz || status=$?
  check "${case} fails" 1 "$status"
done

if [ "$fails" -ne 0 ]; then
  echo "fuzz.sh self-test failed" >&2
  exit 1
fi

echo "fuzz.sh self-test passed"
