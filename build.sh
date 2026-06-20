#!/usr/bin/env bash
#
# Build the digitorn daemon + CLI + workers on Linux, WITH the treesitter
# code-intel (grep call-graph enrichment + full repo-map).
#
# This is the Linux equivalent of build.ps1.
#
# Usage:
#   ./build.sh            # build everything
#   ./build.sh --run      # build, then (re)launch the daemon
#   ./build.sh --no-stop  # build without stopping a running daemon
#

set -euo pipefail
cd "$(dirname "$0")"

RUN=false
NO_STOP=false
for arg in "$@"; do
  case "$arg" in
    --run)      RUN=true ;;
    --no-stop)  NO_STOP=true ;;
    *)          echo "unknown option: $arg"; exit 1 ;;
  esac
done

PKG='github.com/mbathepaul/digitorn'
VERSION=$(git describe --tags --always --dirty 2>/dev/null || true)
[ -z "$VERSION" ] && VERSION='dev'
DATE=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS="-s -w -X ${PKG}/internal/version.Version=${VERSION} -X ${PKG}/internal/version.BuildDate=${DATE}"
BASE_TAGS='treesitter'

# treesitter (code-intel) and onnx (real embeddings) both use cgo.
export CGO_ENABLED=1

# Stop running daemon/workers unless --no-stop (avoids file-lock issues).
if [ "$NO_STOP" = false ]; then
  pkill -f 'digitornd|digitorn-worker' 2>/dev/null || true
  sleep 0.6
fi

mkdir -p bin

TARGETS=(
  "bin/digitornd|./cmd/digitornd|${BASE_TAGS}"
  "bin/digitorn|./cmd/digitorn|${BASE_TAGS}"
  "bin/digitorn-worker|./cmd/digitorn-worker|${BASE_TAGS}"
  "bin/digitorn-worker-llm|./cmd/digitorn-worker-llm|${BASE_TAGS}"
  "bin/digitorn-worker-embeddings|./cmd/digitorn-worker-embeddings|${BASE_TAGS} onnx"
  "bin/digitorn-worker-tokenizer|./cmd/digitorn-worker-tokenizer|${BASE_TAGS}"
)

for entry in "${TARGETS[@]}"; do
  IFS='|' read -r out src tags <<< "$entry"
  echo -e "\033[36mbuilding ${out} (tags: ${tags})\033[0m"
  go build -trimpath -tags "$tags" -ldflags "$LDFLAGS" -o "$out" "$src"
done

echo -e "\033[32mOK - daemon + CLI + workers built (embeddings: +onnx, version: ${VERSION})\033[0m"

if [ "$RUN" = true ]; then
  ./bin/digitornd -config ./bin/config.yaml &
  echo -e "\033[32mlaunched daemon (-config ./bin/config.yaml)\033[0m"
fi
