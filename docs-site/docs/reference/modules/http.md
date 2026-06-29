---
id: http
title: http Module
sidebar_label: http
sidebar_position: 6
description: HTTP client · REST verbs, file upload/download, SSRF-guarded.
---

# http

HTTP client for calling external APIs. All standard verbs, JSON auto-parsing, multipart upload, file download. Every request goes through the SSRF guard · same blocklist as the `web` module.

| Property | Value |
|----------|-------|
| Module id | `http` |
| Version | `1.0.0` |

## Tools

| Tool | Risk | Purpose |
|------|:----:|---------|
| `http.get` | low | GET request. |
| `http.post` | low | POST request. |
| `http.put` | low | PUT request. |
| `http.patch` | low | PATCH request. |
| `http.delete` | low | DELETE request. |
| `http.head` | low | HEAD request (headers only). |
| `http.request` | low | Generic request with explicit `method`. |
| `http.download` | medium | Download URL to a local file path. |
| `http.upload` | medium | Upload a local file via multipart/form-data. |

### Common parameters

| Param | Type | Description |
|-------|------|-------------|
| `url` | string | Target URL. **Required.** |
| `headers` | object | Additional HTTP headers. |
| `body` | string | Request body (auto-detects JSON or plain text). |
| `auth` | string | Named credential from `config.credentials`, or inline `"Bearer <token>"`. |
| `timeout` | integer | Per-request timeout in seconds (default 30). |

### Response shape

```json
{
  "status": 200,
  "ok": true,
  "body": "...",
  "json": {},
  "content_type": "application/json",
  "headers": {},
  "url": "https://..."
}
```

`json` is populated automatically when the response `Content-Type` is `application/json`.

## Authentication

Three credential types supported via named credentials in config:

| Type | Fields |
|------|--------|
| `bearer` | `token` |
| `basic` | `username`, `password` |
| `api_key` | `token`, `header` (default `X-API-Key`) |

Inline auth also works: pass `"Bearer sk-..."` directly in the `auth` param.

## Configuration

```yaml
tools:
  modules:
    http:
      config:
        timeout: 30
        allowed_hosts: [api.stripe.com, api.github.com]
        blocked_hosts: [internal.corp.com]
        default_headers:
          User-Agent: "MyBot/1.0"
        credentials:
          stripe:
            type: bearer
            token: "{{secret.STRIPE_KEY}}"
          internal:
            type: api_key
            token: "{{secret.INTERNAL_KEY}}"
            header: X-Internal-Key
```

## SSRF guard

All URLs are validated before the request is sent · same blocklist as the `web` module: loopback, RFC 1918, link-local, AWS/GCP metadata endpoints, IPv6 ULA. Set `allow_private_hosts: true` to disable (dev only).

## Cross-references

- [web module](web.md) · search + fetch (built on the same SSRF guard)
- [App Configuration → tools.modules](../../language/02-app-config.md#toolsmodules---module-configuration)
