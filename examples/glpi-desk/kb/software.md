# Software — IT Knowledge Base

## Microsoft Office won't open / crashes on launch
1. Close all Office apps.
2. macOS: hold Option and open the app to reset; Windows: run "Quick Repair" from Apps & Features → Microsoft 365 → Modify.
3. Sign out and back in with your ACME email so the license re-activates.
4. Still crashing after repair → escalate to Software L2 with the crash log.

## Outlook / mail client not syncing
1. Verify you are on VPN or on the office network.
2. Remove and re-add the account: server `outlook.office365.com`, your ACME email.
3. Rebuild the local profile if messages are stuck (Outlook → Preferences → Accounts → Rebuild).
4. Shared-mailbox access requests are handled by IT with approval → escalate.

## Application install request
1. Self-service catalog: the ACME Software Center (icon on the desktop) installs approved apps without admin rights.
2. If the app is not in the catalog, it needs a license/security review — this requires **admin rights or a purchase**, so it is escalated to Software L2. Never install it via a reply.

## "You need administrator rights"
Granting admin rights or running privileged installers is **never** actioned from a reply. State that it is escalated to Software L2 for review.
