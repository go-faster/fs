#!/usr/bin/env bash
# Runs every Go fuzz target in the repo for a bounded time. Used by the fuzz
# CI workflow and available locally (`make fuzz`, or FUZZTIME=5m scripts/fuzz.sh).
#
# The seed corpora also run as ordinary unit tests in `go test ./...`, so known
# crashers are permanent regressions regardless of this active search.
set -euo pipefail

FUZZTIME="${FUZZTIME:-60s}"

# Discover "package<TAB>FuzzName" pairs from the fuzz targets in the tree.
mapfile -t targets < <(
  grep -rEl '^func Fuzz[A-Za-z0-9_]+\(' --include='*_test.go' . |
    while read -r file; do
      pkg="./$(dirname "${file#./}")"
      grep -oE '^func (Fuzz[A-Za-z0-9_]+)\(' "$file" | sed -E 's/^func (Fuzz[A-Za-z0-9_]+)\(/\1/' |
        while read -r fn; do printf '%s\t%s\n' "$pkg" "$fn"; done
    done | sort -u
)

if [ "${#targets[@]}" -eq 0 ]; then
  echo "no fuzz targets found" >&2
  exit 1
fi

# deadline_flake classifies a failed run: true when the fuzzer ran out of its
# budget without finding anything.
#
# Every -fuzztime run ends by hitting its deadline, and when a worker is still
# mid-execution as it fires, the coordinator surfaces the shutdown as
# "context deadline exceeded" and a non-zero exit — a finished search reported
# as a failure. It is a property of how the run is stopped, not of the code
# under test, so it happens more on a loaded runner and more with a short
# budget.
#
# Nothing is inferred from the absence of evidence: a crasher writes a corpus
# entry and says so, and a hang or panic prints one. Any of those disqualifies
# the run from being called a flake, whatever else the output contains.
deadline_flake() {
  local out="$1"

  grep -q "context deadline exceeded" "$out" || return 1
  grep -q "Failing input written to" "$out" && return 1
  grep -q "^panic:" "$out" && return 1
  grep -q "test timed out" "$out" && return 1

  return 0
}

out="$(mktemp)"
trap 'rm -f "$out"' EXIT

# run_target fuzzes one target, printing its output. Returns 0 on success, 2 on
# a budget-expiry flake, 1 on a real failure. Output is captured rather than
# streamed so it can be classified; it is echoed either way, so a failing run
# still shows everything the fuzzer said.
run_target() {
  local pkg="$1" fn="$2" status=0

  go test "$pkg" -run '^$' -fuzz "^${fn}\$" -fuzztime "${FUZZTIME}" >"$out" 2>&1 || status=$?

  cat "$out"

  if [ "$status" -eq 0 ]; then
    return 0
  fi

  if deadline_flake "$out"; then
    return 2
  fi

  return 1
}

echo "running ${#targets[@]} fuzz targets for ${FUZZTIME} each"

fail=0
flakes=0

for t in "${targets[@]}"; do
  pkg="${t%%$'\t'*}"
  fn="${t##*$'\t'}"

  echo "::group::${pkg} ${fn}"

  status=0
  run_target "$pkg" "$fn" || status=$?

  if [ "$status" -eq 2 ]; then
    # Retried once rather than tolerated outright. A search stopped by its own
    # deadline passes the second time; anything that keeps ending this way is
    # reproducible, so it fails the build and gets looked at.
    echo "::warning::${pkg} ${fn} ran out its fuzz budget without finding anything; retrying once"

    status=0
    run_target "$pkg" "$fn" || status=$?

    if [ "$status" -eq 2 ]; then
      echo "FUZZ FAILED: ${pkg} ${fn} exceeded its deadline twice — not a flake" >&2
      status=1
    else
      flakes=$((flakes + 1))
    fi
  fi

  if [ "$status" -ne 0 ]; then
    echo "FUZZ FAILED: ${pkg} ${fn}" >&2
    fail=1
  fi

  echo "::endgroup::"
done

if [ "$flakes" -gt 0 ]; then
  echo "${flakes} target(s) needed a retry after their budget expired mid-execution"
fi

exit "$fail"
