#!/usr/bin/env bash
# Compile every YAML example from digitorn-bridge against our Go compiler and
# produce a categorized report.
#
# Skipped:
#   - fragment files (no top-level `app:` block) — meant for include
#   - apps listed in scripts/testignore.txt — they reference modules that
#     are not shipped by the Python daemon either (plugin / MCP only).
set -u

BRIDGE="${BRIDGE:-C:/Users/ASUS/Documents/digitorn-bridge}"
BIN="${BIN:-bin/digitorn.exe}"
OUT_DIR="${OUT_DIR:-/tmp/digitorn-tests}"
IGNORE="${IGNORE:-scripts/testignore.txt}"

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

ok_count=0
fail_count=0
skip_fragments=0
skip_ignored=0
declare -A code_count

export ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-sk-test}"
export OPENAI_API_KEY="${OPENAI_API_KEY:-sk-test}"
export DEEPSEEK_API_KEY="${DEEPSEEK_API_KEY:-sk-test}"
export GROQ_API_KEY="${GROQ_API_KEY:-sk-test}"
export GITHUB_TOKEN="${GITHUB_TOKEN:-dummy}"
export NOTION_TOKEN="${NOTION_TOKEN:-dummy}"
export SLACK_BOT_TOKEN="${SLACK_BOT_TOKEN:-dummy}"
export GMAIL_APP_PASSWORD="${GMAIL_APP_PASSWORD:-dummy}"

# Build the ignore set: strip comments and blank lines.
ignored_set=""
if [ -f "$IGNORE" ]; then
    ignored_set=$(grep -vE '^\s*(#|$)' "$IGNORE" | awk '{print $1}')
fi

is_ignored() {
    [ -z "$ignored_set" ] && return 1
    echo "$ignored_set" | grep -qxF "$1"
}

files=$(find "$BRIDGE/examples" -name "*.yaml" -o -name "*.yml" 2>/dev/null
        find "$BRIDGE/packages/digitorn/builtins" -name "app.yaml" 2>/dev/null)

for f in $files; do
    if ! grep -qE '^app:' "$f"; then
        skip_fragments=$((skip_fragments+1))
        continue
    fi
    rel="${f#$BRIDGE/}"
    if is_ignored "$rel"; then
        skip_ignored=$((skip_ignored+1))
        continue
    fi
    log="$OUT_DIR/$(echo "$rel" | tr '/' '_').log"
    if "$BIN" lint "$f" > "$log" 2>&1; then
        ok_count=$((ok_count+1))
    else
        fail_count=$((fail_count+1))
        codes=$(grep -oE 'DGT-[EW][0-9]+' "$log" | sort -u)
        for code in $codes; do
            code_count[$code]=$((${code_count[$code]:-0}+1))
        done
    fi
done

total=$((ok_count + fail_count))
echo "===================== RESULT ====================="
printf "Skipped (fragments)   : %d\n" "$skip_fragments"
printf "Skipped (.testignore) : %d\n" "$skip_ignored"
printf "Files tested          : %d\n" "$total"
if [ "$total" -gt 0 ]; then
    printf "  OK                  : %d (%d%%)\n" "$ok_count" $((ok_count*100/total))
    printf "  FAILED              : %d (%d%%)\n" "$fail_count" $((fail_count*100/total))
fi

if [ "$fail_count" -gt 0 ]; then
    echo ""
    echo "Diagnostic codes (file count affected):"
    for code in "${!code_count[@]}"; do
        printf "  %-12s %4d\n" "$code" "${code_count[$code]}"
    done | sort -k2 -rn
fi

echo ""
echo "Per-file logs in: $OUT_DIR/"
