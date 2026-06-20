#!/bin/bash
# Build the Digitorn TUI binary from the opencode-fork.
# Usage: ./build-linux.sh [version]
#   version defaults to 0.1.0

set -euo pipefail
VERSION="${1:-0.1.0}"
PKG="/home/paul/codes/digitorn/clients/opencode-fork/packages/opencode"
DIST="/home/paul/codes/digitorn/clients/cli"

echo "▶ Building Digitorn TUI v${VERSION}..."
cd "$PKG"
OPENCODE_VERSION="$VERSION" OPENCODE_CHANNEL=latest bun run script/build.ts

# Copy linux glibc binary to the CLI distribution folder
cp "$PKG/dist/opencode-linux-x64/bin/opencode" "$DIST/opencode"
echo "▶ Binary: $DIST/opencode"
ls -lh "$DIST/opencode"
