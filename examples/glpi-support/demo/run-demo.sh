#!/usr/bin/env bash
# End-to-end local demo: GLPI ticket + Digitorn webhook trigger.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

ENV_FILE="${1:-demo.env}"

echo "=== GLPI + Digitorn demo ==="
echo ""

if [[ ! -f "$ENV_FILE" ]]; then
  cp demo.env.example "$ENV_FILE"
  echo "Created $ENV_FILE from demo.env.example — fill GLPI_APP_TOKEN and GLPI_USER_TOKEN, then re-run."
  exit 1
fi
# shellcheck disable=SC1090
source "$ENV_FILE"

# Digitorn background
if ! curl -sf -o /dev/null "${DIGITORN_BACKGROUND_URL:-http://127.0.0.1:8090}/hook/glpi" -X POST \
  -H "Content-Type: application/json" -H "X-API-Key: probe" -d '{}' 2>/dev/null; then
  if ! curl -sf -o /dev/null "${DIGITORN_BACKGROUND_URL:-http://127.0.0.1:8090}/" 2>/dev/null; then
    echo "digitorn-background not reachable on :8090. Start it first:"
    echo "  cd /path/to/digitorn && ./bin/digitorn-background"
    exit 1
  fi
fi

# GLPI
if ! curl -sf -o /dev/null "http://localhost:8080/" 2>/dev/null; then
  echo "GLPI not running on :8080. Starting Docker stack..."
  "$ROOT/start.sh"
fi

if [[ -z "${GLPI_APP_TOKEN:-}" || -z "${GLPI_USER_TOKEN:-}" ]]; then
  echo ""
  echo "Complete one-time GLPI API setup (see README.md), then set in $ENV_FILE:"
  echo "  GLPI_APP_TOKEN=..."
  echo "  GLPI_USER_TOKEN=..."
  exit 1
fi

echo "Refreshing GLPI session..."
"$ROOT/glpi-session.sh" "$ENV_FILE"

echo "Pushing secrets to Digitorn (or use UI if this fails)..."
"$ROOT/push-secrets.sh" "$ENV_FILE" || true

# Background arms webhook api_key at boot from the daemon secret store — restart
# so a freshly saved GLPI_WEBHOOK_KEY is picked up (runtime POST /ops/triggers
# does not re-arm webhook adapters today).
if pgrep -f './bin/digitorn-background' >/dev/null 2>&1; then
  echo "Restarting digitorn-background to reload webhook secret..."
  pkill -f './bin/digitorn-background' || true
  sleep 2
fi
if [[ -x "${DIGITORN_ROOT:-../../..}/bin/digitorn-background" ]]; then
  (cd "${DIGITORN_ROOT:-../../..}" && ./bin/digitorn-background >> /tmp/digitorn-background.log 2>&1 &)
  sleep 4
fi

echo ""
echo "Creating GLPI ticket + firing webhook..."
TICKET_ID="$("$ROOT/create-ticket.sh" "$ENV_FILE")"
echo "GLPI ticket #${TICKET_ID}"
"$ROOT/fire-webhook.sh" "$ENV_FILE" "$TICKET_ID"
