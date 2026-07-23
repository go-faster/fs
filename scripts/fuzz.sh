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

echo "running ${#targets[@]} fuzz targets for ${FUZZTIME} each"

fail=0
for t in "${targets[@]}"; do
  pkg="${t%%$'\t'*}"
  fn="${t##*$'\t'}"
  echo "::group::${pkg} ${fn}"
  if ! go test "$pkg" -run '^$' -fuzz "^${fn}\$" -fuzztime "${FUZZTIME}"; then
    echo "FUZZ FAILED: ${pkg} ${fn}" >&2
    fail=1
  fi
  echo "::endgroup::"
done

exit "$fail"
