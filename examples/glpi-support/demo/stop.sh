#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"
docker compose down
echo "GLPI demo stack stopped. Data kept in Docker volumes (glpi-demo_glpi_data, glpi-demo_db_data)."
echo "Full wipe: docker compose down -v"
