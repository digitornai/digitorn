# vendor-references

Read-only upstream snapshots kept purely as a reference while building digitorn.
**Nothing here is imported or built** — it's for reading patterns, not linking.

## opencode

Two snapshots are kept on purpose, because opencode rewrote its TUI from Go to
TypeScript between them:

| Snapshot | TUI stack | Use it for |
|----------|-----------|------------|
| `opencode-v0.6.3` | **Go + Bubble Tea + lipgloss** (154 `.go` files) | Direct ports into our Go CLI (`clients/cli`). This is our stack — e.g. the diff renderer in `clients/cli/internal/render/diff.go` was ported from `packages/tui/internal/components/diff/diff.go`. |
| `opencode-v1.15.13` | **TypeScript / `.tsx`** (0 `.go` files) | Newer UX + language-agnostic algorithm ideas (diff-viewer, command palette, dialogs). Cannot be copied verbatim — different language/framework. |

v0.6.3 is the **last opencode release with the Go TUI**; that's why it's pinned
rather than tracking latest. When porting a widget, prefer reading 0.6.3 for the
implementation and 1.15.13 for any refinements to the behaviour/UX.
