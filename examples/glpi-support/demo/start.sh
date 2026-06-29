#!/usr/bin/env bash
# Start local GLPI (Docker). First boot may take 1–2 minutes.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

if ! command -v docker >/dev/null 2>&1; then
  echo "Docker is required. Install Docker Engine + compose plugin first."
  exit 1
fi

echo "Starting GLPI demo stack on http://localhost:8080 ..."
docker compose up -d

echo ""
echo "Waiting for GLPI HTTP..."
for i in $(seq 1 60); do
  if curl -sf -o /dev/null "http://localhost:8080/"; then
    echo "GLPI is up."
    echo ""
    echo "Next:"
    echo "  1. Open http://localhost:8080 (default login often glpi / glpi on first run)"
    echo "  2. Enable REST API + create API client (see README.md § GLPI one-time setup)"
    echo "  3. cp demo.env.example demo.env && fill GLPI_APP_TOKEN + GLPI_USER_TOKEN"
    echo "  4. ./glpi-session.sh && ./push-secrets.sh"
    echo "  5. ./run-demo.sh"
    exit 0
  fi
  sleep 2
done

echo "GLPI did not respond on :8080 within 2 minutes. Check: docker compose logs -f glpi"
exit 1
