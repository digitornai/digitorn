# Network procedures

## VPN cannot connect
1. Confirm the user is off any captive-portal Wi‑Fi (hotel, airport).
2. Verify the VPN client version is >= 7.2; older builds fail the new TLS handshake.
3. Have the user try the backup gateway `vpn-eu2.corp.example`.
4. If it still fails, collect the client log (`Help → Export diagnostics`) and escalate to Network L2.

## No internet on a wired port
- Check the switch port is not in the quarantine VLAN (NAC). A freshly imaged
  machine lands in quarantine until the agent reports compliant.
- Ask the user to confirm the cable is in a *blue* (data) port, not a *white*
  (voice) port.

## DNS resolution issues
- Internal names must resolve via `10.0.0.53`. If the user has a public resolver
  (`8.8.8.8`) pinned, internal sites break. Reset DNS to DHCP.

## Escalation
Outages affecting a whole site, or anything touching the firewall ruleset, must
go to Network L2 — never change firewall rules from a ticket reply.
