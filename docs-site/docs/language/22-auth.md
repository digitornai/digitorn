---
id: auth
title: Authentication
sidebar_label: Authentication
---

The daemon's HTTP authentication surface (`/auth/*`) and the
JWT issuance / refresh / revocation flows are **not part of the
public documentation contract**.

Public clients should use the SDK or CLI to obtain and
refresh tokens automatically. Manual token handling is not
recommended.

| Need | Use |
|------|-----|
| Log in from a script | CLI `digitorn login` or pass `token=` explicitly |
| Log in from the terminal | `digitorn auth login` (CLI) |
| Pair a daemon with a hosted Digitorn account | Via cloud pairing config |

For direct HTTP integration outside of the SDKs, contact your
daemon administrator.

## Per-user installs

`runtime.session_mode: multi` (declared in the
[runtime block](02-app-config.md#runtime---lifecycle-and-execution-policy))
plus the deploy scope (`scope=user` from the JWT, vs.
`scope=system`) is what makes per-user installs work. The
JWT carries the user identity - apps deployed under
`scope=user` are private to the bearer.

See [Multi-Tenant Installs](45-multi-tenant.md) for the
`(app_id, scope, owner_user_id)` semantics.
