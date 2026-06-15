# Deploying the Go daemon

Push to `main` → GitHub Actions builds static `linux/amd64` binaries, ships them
to the Hetzner box, stops the legacy Python daemon, and starts the Go daemon on
`:8000` (behind the existing Caddy vhost `api.digitorn.ai`). Health-checks and
rolls back automatically if the new daemon does not come up.

The box layout mirrors the legacy setup so Caddy and the systemd conventions are
unchanged:

```
/opt/digitorn-go/
  releases/<git-sha>/      # each deploy lands here (digitornd + all workers)
  current -> releases/...  # symlink flipped atomically by deploy.sh
  deploy.sh                # shipped by CI
/etc/digitorn/
  digitorn-go.yaml         # non-secret config (one-time)
  digitorn-go.env          # secrets + instance URLs (one-time, chmod 600)
/etc/systemd/system/digitorn-go.service
```

## 1. One-time server bootstrap (run once, as root on the box)

```bash
# Reuse the existing 'digitorn' service user; create the release tree.
install -d -o digitorn -g digitorn /opt/digitorn-go /opt/digitorn-go/releases
install -d -o digitorn -g digitorn /var/log/digitorn

# Dedicated Postgres database + role for the Go daemon (separate from the old one).
sudo -u postgres psql -c "CREATE ROLE digitorn_go LOGIN PASSWORD 'STRONG_PASSWORD';"
sudo -u postgres psql -c "CREATE DATABASE digitorn_go OWNER digitorn_go;"

# Config + secrets (fill in real values — see the .example files in this repo).
install -d /etc/digitorn
cp digitorn-go.example.yaml /etc/digitorn/digitorn-go.yaml
cp digitorn-go.env.example  /etc/digitorn/digitorn-go.env
chmod 600 /etc/digitorn/digitorn-go.env
# -> edit /etc/digitorn/digitorn-go.env: set the real Postgres DSN, gateway URL,
#    JWKS issuer/url, redis url.

# Passwordless sudo for the deploy user so CI can rsync as root + run deploy.sh.
echo "$DEPLOY_USER ALL=(root) NOPASSWD: /usr/bin/rsync, /opt/digitorn-go/deploy.sh, /bin/systemctl, /usr/bin/systemctl" \
  > /etc/sudoers.d/digitorn-go && chmod 440 /etc/sudoers.d/digitorn-go
```

The systemd unit (`deploy/digitorn-go.service`) is installed automatically by CI
on the first deploy.

## 2. GitHub repo secrets (Settings → Secrets and variables → Actions)

The Go repo (`mbathe/digitorn`) needs its own copy — secrets are per-repo:

| Secret | Value |
|---|---|
| `HETZNER_HOST` | the box hostname / IP |
| `HETZNER_USER` | the deploy SSH user (has the NOPASSWD sudo above) |
| `HETZNER_SSH_KEY` | the deploy user's **private** SSH key |

## 3. Cut over from the old daemon

The Go daemon binds the same `:8000` and serves the same `/health`, so Caddy and
the `api.digitorn.ai` vhost need no change. `deploy.sh` stops `digitorn-daemon`
(the Python service) before starting `digitorn-go`.

To stop the old daemon from being redeployed, disable the deploy workflow in the
**`digitorn-bridge`** repo (rename `deploy.yml` or pause it in the Actions tab).
Leave the old release on disk for a few days as a manual fallback.

## 4. Deploy, watch, roll back

```bash
# Deploy: just push to main (or hit "Run workflow" in the Actions tab).

# Watch logs on the box:
journalctl -u digitorn-go -f

# Manual rollback to the previous release (kept: last 5):
ls -dt /opt/digitorn-go/releases/*/ | sed -n 2p   # the previous one
sudo /opt/digitorn-go/deploy.sh /opt/digitorn-go/releases/<previous-sha>

# Emergency: restore the legacy Python daemon
sudo systemctl stop digitorn-go && sudo systemctl start digitorn-daemon
```

## Notes

- **No CGO**: the binaries are fully static (`CGO_ENABLED=0`), so the box needs
  no Go toolchain and no shared libs.
- **MCP servers**: stdio catalog servers spawn `npx`/`uvx`. Install Node and
  `uv` on the box if apps use npm/pip MCP servers; HTTP/SSE servers need nothing.
- **Arch**: built for `amd64`. If the box is ARM, change `GOARCH=arm64` in
  `.github/workflows/deploy.yml`.
