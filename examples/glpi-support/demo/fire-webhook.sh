#!/usr/bin/env bash
# Simulate GLPI → Digitorn webhook (new ticket event).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

ENV_FILE="${1:-demo.env}"
TICKET_ID="${2:-}"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "Missing $ENV_FILE"
  exit 1
fi
# shellcheck disable=SC1090
source "$ENV_FILE"

: "${DIGITORN_BACKGROUND_URL:=http://127.0.0.1:8090}"
: "${GLPI_WEBHOOK_KEY:=demo-webhook-secret}"

NAME="${DEMO_TICKET_NAME:-VPN ne fonctionne plus}"
CONTENT="${DEMO_TICKET_CONTENT:-Impossible de me connecter au VPN depuis ce matin.}"
CATEGORY="${DEMO_TICKET_CATEGORY:-Network}"
USER_ID="${DEMO_TICKET_USER_ID:-2}"

if [[ -z "$TICKET_ID" ]]; then
  if [[ -x "$ROOT/create-ticket.sh" ]]; then
    echo "No ticket id — creating one in GLPI..."
    TICKET_ID="$("$ROOT/create-ticket.sh" "$ENV_FILE")"
    echo "Created GLPI ticket #${TICKET_ID}"
  else
    TICKET_ID="4242"
    echo "Using dummy ticket id ${TICKET_ID} (write-back will fail unless it exists in GLPI)"
  fi
fi

payload="$(TICKET_ID="$TICKET_ID" NAME="$NAME" CONTENT="$CONTENT" CATEGORY="$CATEGORY" USER_ID="$USER_ID" python3 - <<'PY'
import json, os
print(json.dumps({
    "id": int(os.environ["TICKET_ID"]),
    "status": "new",
    "name": os.environ["NAME"],
    "content": os.environ["CONTENT"],
    "itilcategories_name": os.environ["CATEGORY"],
    "users_id": int(os.environ["USER_ID"]),
}))
PY
)"

code="$(curl -s -o /tmp/digitorn-webhook-resp.json -w '%{http_code}' \
  -X POST "${DIGITORN_BACKGROUND_URL%/}/hook/glpi" \
  -H "Content-Type: application/json" \
  -H "X-API-Key: ${GLPI_WEBHOOK_KEY}" \
  -d "$payload")"

echo "Webhook POST → HTTP ${code}"
cat /tmp/digitorn-webhook-resp.json
echo ""

if [[ "$code" != "202" && "$code" != "200" ]]; then
  echo "Expected 202 Accepted. Check:"
  echo "  - digitorn-background is running (:8090)"
  echo "  - GLPI_WEBHOOK_KEY matches Digitorn Channels tab"
  exit 1
fi

echo ""
echo "Open Digitorn → /agents/glpi-support → Executions + Approvals"
echo "Approve the reply, then check GLPI ticket #${TICKET_ID} for the follow-up."
