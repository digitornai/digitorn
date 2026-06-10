#!/usr/bin/env bash
# Activate a freshly-rsynced release of the Go daemon: flip the `current`
# symlink, stop the legacy Python daemon to free :8000, restart the Go service,
# health-check, and roll back automatically if it does not come up.
#
# Invoked by the GitHub Actions deploy over SSH as:
#   sudo /opt/digitorn-go/deploy.sh /opt/digitorn-go/releases/<sha>
set -euo pipefail

BASE=/opt/digitorn-go
NEW="${1:?usage: deploy.sh <release-dir>}"
HEALTH_URL="http://127.0.0.1:8000/health"
OLD_SERVICE=digitorn-daemon   # the legacy Python daemon on the same box
NEW_SERVICE=digitorn-go

if [ ! -x "$NEW/digitornd" ]; then
  echo "::error:: $NEW/digitornd missing or not executable" >&2
  exit 1
fi

PREV=""
if [ -L "$BASE/current" ]; then
  PREV="$(readlink -f "$BASE/current" || true)"
fi

chown -R digitorn:digitorn "$NEW"
chmod +x "$NEW/digitornd" "$NEW"/digitorn-worker* 2>/dev/null || true

ln -sfn "$NEW" "$BASE/current"

# Free :8000 from the legacy daemon, then bring up the Go daemon.
systemctl stop "$OLD_SERVICE" 2>/dev/null || true
systemctl daemon-reload
systemctl enable "$NEW_SERVICE" >/dev/null 2>&1 || true
systemctl restart "$NEW_SERVICE"

ok=0
for _ in $(seq 1 20); do
  if curl -fsS --max-time 3 "$HEALTH_URL" 2>/dev/null | grep -q '"status":"ok"'; then
    ok=1; break
  fi
  sleep 1
done

if [ "$ok" -ne 1 ]; then
  echo "::error:: $NEW_SERVICE did not become healthy — rolling back" >&2
  systemctl stop "$NEW_SERVICE" 2>/dev/null || true
  if [ -n "$PREV" ] && [ "$PREV" != "$NEW" ] && [ -x "$PREV/digitornd" ]; then
    ln -sfn "$PREV" "$BASE/current"
    systemctl start "$NEW_SERVICE" || true
  else
    # No good previous Go release: fully restore the legacy daemon so prod
    # survives this AND any subsequent reboot.
    systemctl enable --now "$OLD_SERVICE" || true
  fi
  exit 1
fi

# Cutover succeeded: disable the legacy daemon so a reboot brings up only the Go
# daemon (no :8000 contention). A future rollback re-enables it above.
systemctl disable "$OLD_SERVICE" 2>/dev/null || true

# Keep the last 5 releases for fast manual rollback; prune the rest.
ls -1dt "$BASE"/releases/*/ 2>/dev/null | tail -n +6 | xargs -r rm -rf

echo "deploy ok: $NEW"
