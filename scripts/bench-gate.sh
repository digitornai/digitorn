#!/usr/bin/env bash
# Phase 8 — CI bench gate.
#
# Runs the hot-path benchmarks and fails CI if any regresses past a
# fixed ns/op or allocs/op ceiling. The ceilings are set well above the
# measured numbers (≥2× headroom) so transient CI noise doesn't flake,
# but a real regression (a stray heap-escape, a lock added to the hot
# path) trips immediately.
#
# Usage: bash scripts/bench-gate.sh
# Exits non-zero on regression.

set -euo pipefail

PKG="./internal/llm/bifrost/"
OUT=$(mktemp)
trap 'rm -f "$OUT"' EXIT

echo ">> running benches (3 iterations, 1s per bench)..."
go test -bench '^Benchmark(RouteInfo|Dispatch)' -benchmem -run '^$' \
    -count=3 -benchtime=1s "$PKG" | tee "$OUT"

# Ceilings — kept loose enough to survive CI variance, tight enough to
# catch real regressions. Adjust ONLY when intentional architectural
# change makes the previous baseline obsolete.
declare -A NS_CEIL=(
    [BenchmarkRouteInfoPool]=20
    [BenchmarkDispatch_FastPath]=500
    [BenchmarkDispatch_SaturatedAdmission]=400
    [BenchmarkDispatch_10KConcurrent]=500
)
declare -A ALLOC_CEIL=(
    [BenchmarkRouteInfoPool]=0
    [BenchmarkDispatch_FastPath]=0
    [BenchmarkDispatch_SaturatedAdmission]=0
    [BenchmarkDispatch_10KConcurrent]=0
)

fail=0
for name in "${!NS_CEIL[@]}"; do
    # Average ns/op across the 3 runs.
    ns=$(grep -E "^${name}-[0-9]+" "$OUT" | awk '{print $3}' | awk '{s+=$1; n++} END {if(n>0) print s/n; else print 0}')
    allocs=$(grep -E "^${name}-[0-9]+" "$OUT" | awk '{print $7}' | awk '{s+=$1; n++} END {if(n>0) print s/n; else print 0}')
    ns_cap=${NS_CEIL[$name]}
    al_cap=${ALLOC_CEIL[$name]}
    # bash arithmetic doesn't do float — use awk for the compare.
    if awk -v v="$ns"     -v c="$ns_cap" 'BEGIN{exit !(v > c)}'; then
        echo "FAIL: $name ${ns} ns/op > cap ${ns_cap}"
        fail=1
    fi
    if awk -v v="$allocs" -v c="$al_cap" 'BEGIN{exit !(v > c)}'; then
        echo "FAIL: $name ${allocs} allocs/op > cap ${al_cap}"
        fail=1
    fi
    printf "OK   %-45s  ns=%8.1f (cap %d)  allocs=%g (cap %d)\n" \
        "$name" "$ns" "$ns_cap" "$allocs" "$al_cap"
done
exit "$fail"
