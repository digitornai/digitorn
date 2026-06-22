#!/usr/bin/env bash
# Build the opencode-fork TUI for every release platform. $1 is the version.
# Called by .github/workflows/release.yml (build-tui job) before the Go matrix build.
# Output goes to <project-root>/.goreleaser-tui/{os}/{arch}/digitorn-tui
set -euo pipefail

VERSION="${1:-0.1.0}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TUI_PKG="$ROOT/clients/opencode-fork/packages/opencode"
OUT_DIR="$ROOT/.goreleaser-tui"

# Install unzip + bun if not available (e.g. inside the goreleaser-cross Docker image)
if ! command -v bun &>/dev/null; then
    echo "▶ Installing bun..."
    apt-get update -qq && apt-get install -y -qq unzip 2>/dev/null || true
    curl -fsSL https://bun.sh/install | bash
    export PATH="$HOME/.bun/bin:$PATH"
fi

declare -A TARGETS
TARGETS["linux/amd64"]="linux-x64"
TARGETS["linux/arm64"]="linux-arm64"
TARGETS["darwin/amd64"]="darwin-x64"
TARGETS["darwin/arm64"]="darwin-arm64"
TARGETS["windows/amd64"]="windows-x64"
TARGETS["windows/arm64"]="windows-arm64"

echo "▶ Building TUI binaries v${VERSION} for all platforms..."

cd "$TUI_PKG"
OPENCODE_VERSION="$VERSION" OPENCODE_CHANNEL=latest bun run script/build.ts

for key in "${!TARGETS[@]}"; do
    IFS='/' read -r os arch <<< "$key"
    dist_suffix="${TARGETS[$key]}"

    mkdir -p "$OUT_DIR/$os/$arch"
    src="$TUI_PKG/dist/opencode-$dist_suffix/bin"
    if [ -f "$src/opencode" ]; then
        cp "$src/opencode" "$OUT_DIR/$os/$arch/digitorn-tui"
        echo "  ✓ $os/$arch"
    elif [ -f "$src/opencode.exe" ]; then
        cp "$src/opencode.exe" "$OUT_DIR/$os/$arch/digitorn-tui.exe"
        echo "  ✓ $os/$arch.exe"
    else
        echo "  ✗ $os/$arch — binary not found in $src"
    fi
done

echo "▶ TUI build complete"
