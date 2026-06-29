# Accounts & access

## Password reset
- Self-service reset is at `https://reset.corp.example`. Reset links expire after
  1 hour.
- If the account is locked after failed attempts, it auto-unlocks in 15 minutes.
  Manual unlock requires identity verification.

## New access / group membership
- Access to a shared drive or app group requires the resource owner's approval.
  Never grant access from a ticket reply — open an access request for the owner.

## Multi-factor authentication
- Lost phone / new device: the user re-enrolls from `https://mfa.corp.example`
  after verifying identity. Backup codes work once each.

## Offboarding
- Disabling an account or revoking access on departure is a privileged action
  handled by IT Security — escalate, do not action from a reply.

## Escalation
Anything that grants or removes access, or touches a privileged/admin account,
goes to IT Security with human approval.
