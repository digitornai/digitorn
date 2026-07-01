# Troubleshooting

## Login problems

- **"Invalid credentials"**: reset your password from the login page. Reset
  links expire after 1 hour.
- **SSO loop**: clear cookies for `app.lumina.io`, then sign in again. If your
  company uses SAML, an admin must re-authorize the app in your IdP.
- **2FA lockout**: use a backup code. If none remain, support can disable 2FA
  after identity verification.

## App won't load / blank screen

1. Hard refresh (Ctrl/Cmd + Shift + R).
2. Disable browser extensions (ad blockers can break the realtime sync).
3. Check status.lumina.io for ongoing incidents.

## Sync / data not updating

Lumina syncs over websockets. A corporate firewall blocking `wss://` causes
stale boards. Whitelist `*.lumina.io` on ports 443.

## Exporting data

**Settings → Workspace → Export** produces a JSON or CSV archive. Large
workspaces (>50k items) are emailed as a download link within an hour.

## Reporting a bug

Include: the workspace URL, the time it happened, your browser, and a
screenshot. Critical bugs (data loss, security) are escalated immediately.
