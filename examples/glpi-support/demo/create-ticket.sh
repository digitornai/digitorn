#!/usr/bin/env bash
# Create a ticket in local GLPI via REST API. Prints the new ticket id.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

ENV_FILE="${1:-demo.env}"
if [[ ! -f "$ENV_FILE" ]]; then
  echo "Missing $ENV_FILE"
  exit 1
fi
# shellcheck disable=SC1090
source "$ENV_FILE"

: "${GLPI_URL:?Set GLPI_URL in $ENV_FILE}"
: "${GLPI_APP_TOKEN:?Set GLPI_APP_TOKEN in $ENV_FILE}"
: "${GLPI_SESSION_TOKEN:?Run ./glpi-session.sh first}"

NAME="${DEMO_TICKET_NAME:-VPN ne fonctionne plus}"
CONTENT="${DEMO_TICKET_CONTENT:-Impossible de me connecter au VPN depuis ce matin.}"
USER_ID="${DEMO_TICKET_USER_ID:-2}"

body="$(NAME="$NAME" CONTENT="$CONTENT" USER_ID="$USER_ID" python3 - <<'PY'
import json, os
print(json.dumps({"input": {
    "name": os.environ["NAME"],
    "content": os.environ["CONTENT"],
    "type": 1,
    "status": 1,
    "urgency": 3,
    "requesttypes_id": 1,
    "_users_id_requester": int(os.environ["USER_ID"]),
    "entities_id": 0,
}}))
PY
)"

resp="$(curl -sf -X POST "${GLPI_URL%/}/apirest.php/Ticket" \
  -H "App-Token: ${GLPI_APP_TOKEN}" \
  -H "Session-Token: ${GLPI_SESSION_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "$body")"

id="$(echo "$resp" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('id',''))" 2>/dev/null || true)"
if [[ -z "$id" ]]; then
  id="$(echo "$resp" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -1)"
fi
if [[ -z "$id" ]]; then
  echo "Ticket creation failed:"
  echo "$resp"
  exit 1
fi

echo "$id"
