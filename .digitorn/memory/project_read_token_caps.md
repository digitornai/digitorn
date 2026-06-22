---
name: "token-efficient-read"
description: "read tool caps reduced to save context tokens — treeMaxEntries=300, outlineCap=50"
metadata:
  type: project
  originSessionId: current
---

In `internal/modules/filesystem/tree.go`, the read tool's output caps were reduced to avoid blowing up the context window on large repos:

- `treeMaxEntries`: 20000 → **300** (max files shown in `read .` tree)
- `dirOutlineMaxFiles` / `outlineCap`: 2000 → **50** (max files in `read . outline:true`)
- `dirOutlineFileCap` unchanged at 512KB

**Why:** A `read .` on a 5000+ file repo was generating ~100k tokens, filling the context instantly. With 300 entries, output stays under ~10k tokens.

**How to apply:** When exploring large codebases, always `read` a specific subdirectory (e.g. `read clients/opencode-fork/packages/app/src/`) instead of the root. The truncation message guides the agent to drill deeper.