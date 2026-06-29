# Image Support - Complete Specification

## Implementation Status: COMPLETE

All core components implemented and tested:

| Component                       | Status |
|---------------------------------|:------:|
| ImageStore (disk storage)       | Done   |
| Multimodal messages             | Done   |
| Messages surface accepts images | Done   |
| Image fetch surface             | Done   |
| Anthropic provider vision       | Done   |
| OpenAI provider vision          | Done   |
| filesystem.read images          | Done   |
| agent_loop image injection      | Done   |
| Socket.IO image events          | Done   |
| Image aging                     | Done   |
| YAML vision config              | Done   |
| Daemon image config             | Done   |

## Overview

Support for images at every level of the runtime:
- **User -> Agent**: the user sends images (upload, paste, URL).
- **Tool -> Agent**: a tool produces an image (screenshot, diagram, chart).
- **Agent -> User**: images are rendered in the chat.

## Ecosystem reference

### Claude Code (current capabilities)
- Cmd+V pastes a screenshot into the chat.
- The Read tool does NOT read images from the filesystem.
- The agent cannot capture screenshots on its own.

### Anthropic API (Claude)
- Formats: JPEG, PNG, GIF, WebP.
- Max: 8000x8000 px, 100 images per request (200K context).
- **Best practice**: use the Files API for recurring images
  (upload once, reference by `file_id` after).

### OpenAI API (GPT-4o)
- Formats: PNG, JPEG, WebP, non-animated GIF.
- Max: 50MB payload, 500 images per request.
- `detail: "low"` (512px) or `"high"` (native) trades cost vs quality.

### DeepSeek
- DeepSeek-chat (V3): no vision.
- DeepSeek-VL: separate vision model (7B, 1.3B).
- The standard deepseek-chat API does NOT accept images.

---

## Architecture

### Design principles

1. **Images do NOT live in the messages** - they are stored on
   disk and referenced by an `image_id`. Inflated to base64
   only at LLM-call time (most recent turn only for older
   images).

2. **Unified format** - a `ContentBlock` abstracts differences
   between providers. Anthropic/OpenAI conversion happens in
   the provider, not the agent loop.

3. **Tools can return images** - `ActionResult` supports image
   blocks in metadata. The agent loop injects them into the
   messages.

4. **The client receives images over Socket.IO** - no separate
   routes needed; images are inline (base64) in events on the
   `/events` namespace.

---

## 1. Image store (disk storage)

The daemon stores images on disk, referenced by a unique
`image_id`. When an image is sent, the daemon saves it to
`~/.digitorn/images/<session_id>/` and returns a lightweight
reference. The full base64 data is inflated only when needed
for LLM consumption.

### Why not base64 inside messages?

A PNG screenshot is 500KB-2MB base64. Over 10 turns with 3 images each:
- Base64 inside messages = 30MB in memory, re-sent on every LLM call.
- Reference + injection on-demand = a few KB in memory.

### Injection strategy

| Turn | Current-turn images | Previous-turn images |
|------|:-:|:-:|
| Current turn | full base64 (high resolution) | - |
| Turn N-1 | low-resolution base64 (resized to 512px) | - |
| Turn N-2+ | Text: "[Image: screenshot of login page, 1920x1080]" | - |

Keeps the context light while still giving the LLM vision over recent images.

---

## 2. Message Format (multimodal)

Messages support multimodal content: the `content` field can be
either a plain string (backward compatible) or a list of content
blocks. Each block is one of:

- **text** - plain text
- **image** - inline base64 image with media type
- **image_ref** - reference to an image in the store, with
  optional alt text for aging context

---

## 3. Sending images with a message

The daemon's messages surface accepts images alongside the
text body, either as multipart upload (one or more
`images[]` parts plus optional `workspace`) or as JSON with
base64 payloads. The exact route shape is not documented
publicly; clients use the SDK.

### Limits

| Setting | Value | Configurable |
|-----------|--------|:---:|
| Max images per message | 10 | Yes (`images.max_per_message`) |
| Max size per image | 10MB | Yes (`images.max_size_bytes`) |
| Accepted formats | PNG, JPEG, WebP, GIF | No |
| Max total images per session | 100 | Yes (`images.max_per_session`) |

---

## 4. LLM provider - multimodal conversion

Each provider converts the unified content blocks into its
native format internally. The agent loop never touches
provider-specific formats.

### Provider compatibility

| Provider | Vision | Format |
|----------|:---:|--------|
| Claude (Anthropic) | Yes | Native content blocks |
| GPT-4o (OpenAI) | Yes | image_url format |
| GPT-4o-mini | Yes | Same format |
| DeepSeek-chat (V3) | No | Converted to text `[Image: ...]` |
| DeepSeek-VL | Yes | OpenAI-compat format |
| Ollama (llava) | Yes | Special format |
| Ollama (text-only) | No | Converted to text |

Detection is automatic via the provider. Each provider knows
whether its model supports vision.

---

## 5. Tools - images as input and output

### Filesystem: Read image

The `filesystem.read` tool handles images transparently: when
the path points to an image file, it returns the image data
in the tool result metadata instead of text. The agent loop
automatically injects this into the LLM context.

### Browser: Screenshot

The browser module can capture screenshots. The image data is
returned in the tool result and injected into the agent's context.

### Agent Loop - auto injection

When a tool result contains an image, the agent loop
automatically adds it as a content block to the conversation,
making it visible to the LLM on the next turn.

---

## 6. Socket.IO Events - Images to the client

The daemon emits image events on the Socket.IO `/events` namespace.
Images arrive inside `tool_call` envelopes with `image_data`
(base64) and `image_mime` added to the payload. A separate
`image_message` event carries images embedded in agent replies.

---

## 7. Persistence - Images in history

Session history returns images as references (not full base64).
The daemon exposes a per-session image-fetch endpoint that
returns the raw bytes. Clients lazy-load images on demand.

---

## 8. Context optimisation

### Problem

A 1920x1080 PNG base64 weighs ~1-2MB ~= 500K estimated tokens.
If every message has an image, the context explodes in 3 turns.

### Solution: Image Aging

The daemon manages image resolution over time:

| Turn age | Resolution | Estimated tokens |
|-----------|:---:|:---:|
| Current turn | High resolution (1920px) | ~300K |
| 1-2 turns ago | Low resolution (512px) | ~30K |
| 3+ turns ago | Text description | ~25 |

With image aging: 1 image full + 2 low-res + N descriptions = ~360K tokens max.
Without aging: N full images = N x 300K, context explosion.

---

## 9. Config

Settings in `~/.digitorn/config.yaml`:

```yaml
images:
  max_per_message: 10           # Max images per message
  max_size_bytes: 10485760      # 10MB per image
  max_per_session: 100          # Max images per session
  storage_dir: ""               # Empty = ~/.digitorn/images/
  low_res_size: 512             # Size for aged images (px)
  aging_full_turns: 1           # Turns kept at high resolution
  aging_low_turns: 2            # Turns kept at low resolution
  cleanup_after_days: 7         # Delete images after N days
```

---

## 10. YAML App Config

```yaml
agents:
  - id: main
    brain:
      provider: anthropic
      model: claude-sonnet-4-5
      vision: true              # Enable vision support (default: auto-detect)
```

If `vision: false` or the model lacks vision, images are
automatically converted to text descriptions.
