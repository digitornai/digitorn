#!/usr/bin/env bash
# One-time GLPI API bootstrap for the local Docker demo stack.
# Enables REST API, creates digitorn-demo client + user token for glpi (id=2).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

CONTAINER="${GLPI_CONTAINER:-glpi-demo-glpi-1}"
ENV_FILE="${1:-demo.env}"

if ! docker ps --format '{{.Names}}' | grep -qx "$CONTAINER"; then
  echo "GLPI container '$CONTAINER' not running. Run ./start.sh first."
  exit 1
fi

echo "Configuring GLPI API in $CONTAINER ..."
docker exec "$CONTAINER" php /var/www/glpi/bin/console config:set enable_api 1 -n >/dev/null
docker exec "$CONTAINER" php /var/www/glpi/bin/console config:set enable_api_rest 1 -n >/dev/null

read -r GLPI_APP_TOKEN GLPI_USER_TOKEN <<EOF
$(docker exec "$CONTAINER" php -r '
require "/var/www/glpi/vendor/autoload.php";
$kernel = new \Glpi\Kernel\Kernel("production");
$kernel->boot();
$client = new APIClient();
$input = [
  "name" => "digitorn-demo",
  "is_active" => 1,
  "is_recursive" => 1,
  "entities_id" => 0,
  "ipv4_range_start" => 0,
  "ipv4_range_end" => 4294967295,
  "_reset_app_token" => 1,
];
$existing = $client->find(["name" => "digitorn-demo"]);
if ($existing) {
  $id = array_key_first($existing);
  $client->update(["id" => $id] + $input);
} else {
  $id = $client->add($input);
}
$client->getFromDB($id);
$plainApp = (new GLPIKey())->decrypt($client->fields["app_token"]);
$user = new User();
$user->getFromDB(2);
$user->update(["id" => 2, "_reset_api_token" => 1]);
$user->getFromDB(2);
$plainUser = (new GLPIKey())->decrypt($user->fields["api_token"]);
echo $plainApp . " " . $plainUser;
')
EOF

if [[ ! -f "$ENV_FILE" ]]; then
  cp demo.env.example "$ENV_FILE"
fi

upsert() {
  local key="$1" val="$2"
  if grep -q "^${key}=" "$ENV_FILE"; then
    sed -i "s|^${key}=.*|${key}=${val}|" "$ENV_FILE"
  else
    echo "${key}=${val}" >>"$ENV_FILE"
  fi
}

upsert GLPI_URL "http://localhost:8080"
upsert GLPI_APP_TOKEN "$GLPI_APP_TOKEN"
upsert GLPI_USER_TOKEN "$GLPI_USER_TOKEN"
upsert GLPI_WEBHOOK_KEY "demo-webhook-secret"

echo "Wrote GLPI_APP_TOKEN + GLPI_USER_TOKEN to $ENV_FILE"
echo "Next: ./glpi-session.sh && ./push-secrets.sh && restart digitorn-background (see run-demo.sh)"
