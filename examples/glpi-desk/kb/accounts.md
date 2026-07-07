# Accounts & Access — IT Knowledge Base

## Password reset / forgotten password
1. Self-service: https://passwordreset.acme.com — verify with the Microsoft Authenticator push.
2. Choose a passphrase of 4 random words; do not reuse a previous password.
3. Update the password on your phone's mail and Wi-Fi profiles afterwards.
4. If self-service fails (no enrolled MFA), identity must be verified by IT → escalate to IT Security.

## Account locked out
1. Lockouts auto-clear after 15 minutes — wait, then sign in with the correct password.
2. A stale password cached on a phone or a mapped drive often re-locks the account; update it everywhere.
3. Repeated lockouts within an hour → escalate to IT Security (possible credential-stuffing).

## MFA / Authenticator issues
1. Re-add the account in Microsoft Authenticator from https://aka.ms/mfasetup.
2. New phone: register the new device first, then remove the old one.
3. Lost device with no backup method → identity verification required → escalate to IT Security.

## Access request (shared drive, group, application role)
Granting or removing access, adding someone to a security group, or touching a privileged account is **never** done from a reply. State that the request is routed to IT Security for approval, with the resource and the business justification.

## Offboarding
Disabling accounts and revoking access for a departing employee is a controlled process owned by IT Security and HR → escalate; do not action from a reply.
