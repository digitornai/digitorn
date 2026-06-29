#!/usr/bin/env bash
# Push demo secrets to Digitorn (optional if DIGITORN_TOKEN is set).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

ENV_FILE="${1:-demo.env}"
if [[ ! -f "$ENV_FILE" ]]; then
  echo "Missing $ENV_FILE — run: cp demo.env.example demo.env"
  exit 1
fi
# shellcheck disable=SC1090
source "$ENV_FILE"

APP_ID="${DIGITORN_APP_ID:-glpi-support}"
BASE="${DIGITORN_DAEMON_URL:-http://127.0.0.1:8000}"

auth=()
if [[ -n "${DIGITORN_TOKEN:-}" ]]; then
  auth=(-H "Authorization: Bearer ${DIGITORN_TOKEN}")
fi

put_secret() {
  local key="$1" val="$2"
  local code
  code="$(curl -s -o /tmp/digitorn-secret-put.json -w '%{http_code}' -X PUT \
    "${auth[@]}" \
    -H "Content-Type: application/json" \
    -d "{\"value\":$(python3 -c "import json,sys; print(json.dumps(sys.argv[1]))" "$val")}" \
    "${BASE}/api/apps/${APP_ID}/secrets/${key}")"
  if [[ "$code" == "200" || "$code" == "201" ]]; then
    echo "  OK  ${key}"
    return 0
  fi
  echo "  FAIL ${key} (HTTP ${code}) — set manually in Digitorn UI"
  cat /tmp/digitorn-secret-put.json 2>/dev/null || true
  echo ""
  return 1
}

echo "Pushing secrets to ${BASE}/api/apps/${APP_ID}/secrets/ ..."
fail=0
for key in GLPI_URL GLPI_APP_TOKEN GLPI_SESSION_TOKEN GLPI_WEBHOOK_KEY; do
  val="${!key:-}"
  if [[ -z "$val" ]]; then
    echo "  SKIP ${key} (empty in $ENV_FILE)"
    fail=1
    continue
  fi
  put_secret "$key" "$val" || fail=1
done

if [[ "$fail" -ne 0 ]]; then
  echo ""
  echo "Some secrets were not pushed. Use the Digitorn UI instead:"
  echo "  /agents/${APP_ID} → Channels (GLPI_WEBHOOK_KEY)"
  echo "  /agents/${APP_ID} → menu ⋮ → Secrets (GLPI_URL, GLPI_APP_TOKEN, GLPI_SESSION_TOKEN)"
  exit 1
fi

echo "Done. Reload app if you changed YAML: curl -X POST ${BASE}/api/apps/${APP_ID}/reload"
