#!/usr/bin/env bash
# Refresh GLPI_SESSION_TOKEN via initSession and write it to demo.env
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

: "${GLPI_URL:?Set GLPI_URL in $ENV_FILE}"
: "${GLPI_APP_TOKEN:?Set GLPI_APP_TOKEN in $ENV_FILE}"
: "${GLPI_USER_TOKEN:?Set GLPI_USER_TOKEN in $ENV_FILE}"

resp="$(curl -sf -X GET "${GLPI_URL%/}/apirest.php/initSession" \
  -H "App-Token: ${GLPI_APP_TOKEN}" \
  -H "Authorization: user_token ${GLPI_USER_TOKEN}")"

token="$(echo "$resp" | sed -n 's/.*"session_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
if [[ -z "$token" ]]; then
  echo "initSession failed. Response:"
  echo "$resp"
  exit 1
fi

if grep -q '^GLPI_SESSION_TOKEN=' "$ENV_FILE"; then
  sed -i "s|^GLPI_SESSION_TOKEN=.*|GLPI_SESSION_TOKEN=${token}|" "$ENV_FILE"
else
  echo "GLPI_SESSION_TOKEN=${token}" >>"$ENV_FILE"
fi

echo "GLPI_SESSION_TOKEN updated in $ENV_FILE (valid until GLPI session expires)."
