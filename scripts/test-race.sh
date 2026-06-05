#!/usr/bin/env bash
# Run the daemon's test suite under the Go race detector.
#
# The race detector requires CGO + a C compiler ; on Windows we skip and
# rely on Linux/macOS (CI or WSL) to run this script.
set -euo pipefail

if ! command -v gcc >/dev/null 2>&1; then
    echo "ERROR: gcc not found ; the Go race detector requires CGO."
    echo "  Install gcc (Linux: apt install gcc, macOS: xcode-select --install,"
    echo "              Windows: use WSL or install mingw64)"
    exit 1
fi

export CGO_ENABLED=1

echo "=== Race-detected tests (sessionstore + server) ==="
go test -race -count=1 -timeout 5m \
    ./internal/runtime/sessionstore/... \
    ./internal/server/...

echo
echo "=== Race-detected tests with stress suite (longer) ==="
go test -race -count=1 -timeout 10m -run "TestStress" \
    ./internal/server/...

echo
echo "=== All-package smoke under race ==="
go test -race -count=1 -timeout 5m -short ./...

echo "race detector run complete"
