# Network — IT Knowledge Base

## VPN cannot connect
1. Confirm the corporate Wi-Fi or a wired connection is up (open https://intranet.acme.local).
2. Quit the GlobalProtect client fully (system tray → Quit), then relaunch it.
3. Portal address must be `vpn.acme.com`. If it shows anything else, correct it.
4. Sign in with your ACME email and network password (not the app password).
5. If it still fails with "GlobalProtect connection failed", your certificate may be expired — this is an L2 action; the ticket is escalated to Network L2.

## Wi-Fi drops or is slow
1. Forget the `ACME-Corp` network and reconnect (password is rotated monthly; the current one is on the intranet MOTD).
2. Prefer 5 GHz: the SSID `ACME-Corp-5G` is faster in the office.
3. Move within 15 m of an access point; concrete walls degrade the signal.
4. Persistent drops on a specific floor are an infrastructure issue → escalate to Network L2.

## DNS / cannot reach an internal site
1. Internal sites (`*.acme.local`) require the VPN to be connected first.
2. Flush DNS: macOS `sudo dscacheutil -flushcache`; Windows `ipconfig /flushdns`.
3. If a public site works but internal ones do not, the split-tunnel profile is wrong → escalate to Network L2.

## Firewall / port access request
Opening a firewall port or allowing an outbound destination is a **privileged action**. Never provide steps — state that it is escalated to Network L2 with the requested host/port for review.
