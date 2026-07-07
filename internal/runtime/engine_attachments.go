package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// agentCanReadFiles reports whether the agent has the filesystem `read` tool, so
// materialised attachments can be read on demand (read→vision) instead of being
// inlined into every prompt. Empty tool list = all filesystem tools (read too).
func agentCanReadFiles(agent *schema.Agent) bool {
	if agent == nil {
		return false
	}
	for _, m := range agent.Modules {
		if m.ID != "filesystem" {
			continue
		}
		if len(m.Tools) == 0 {
			return true
		}
		for _, t := range m.Tools {
			if t == "read" {
				return true
			}
		}
	}
	return false
}

// attachmentsDir is the folder, relative to the session workdir, where the
// user's message attachments are materialised so the agent can read them with
// its filesystem tools.
const attachmentsDir = "attachments"

// latestUserAttachments returns the attachments of the most recent user message
// (the files the user attached for this turn).
func latestUserAttachments(snap sessionstore.SessionSnapshot) []sessionstore.BlobRef {
	for i := len(snap.Messages) - 1; i >= 0; i-- {
		if snap.Messages[i].Role == "user" {
			return snap.Messages[i].Attachments
		}
	}
	return nil
}

// attachmentDescriptions describes the latest user message's attachments for
// the turn context: "attachments/<name> (<mime>, <size>)". Gives the model
// enough to decide what to open with `read` without loading bytes.
func attachmentDescriptions(snap sessionstore.SessionSnapshot) []string {
	atts := latestUserAttachments(snap)
	if len(atts) == 0 {
		return nil
	}
	out := make([]string, 0, len(atts))
	for _, a := range atts {
		rel := filepath.ToSlash(filepath.Join(attachmentsDir, safeAttachmentName(a)))
		var meta []string
		if a.Mime != "" {
			meta = append(meta, a.Mime)
		}
		if a.Size > 0 {
			meta = append(meta, humanSize(a.Size))
		}
		if len(meta) > 0 {
			rel += " (" + strings.Join(meta, ", ") + ")"
		}
		out = append(out, rel)
	}
	return out
}

// humanSize renders a byte count as B/KB/MB.
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// materializeAttachments writes the latest user message's attachments into
// <workdir>/attachments/<name> so the agent can open them with `read`. It is a
// best-effort, idempotent side effect: no workdir or no blob store → no-op;
// files that already exist are skipped (content is addressed by hash).
func (e *Engine) materializeAttachments(ctx context.Context, snap sessionstore.SessionSnapshot) {
	wd := strings.TrimSpace(snap.Workdir)
	if wd == "" || e.Blobs == nil {
		return
	}
	atts := latestUserAttachments(snap)
	if len(atts) == 0 {
		return
	}
	dir := filepath.Join(wd, attachmentsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	for _, a := range atts {
		data, err := e.Blobs.LoadBlob(ctx, a.Hash)
		if err != nil || len(data) == 0 {
			continue
		}
		// Always (over)write: a re-attached file may reuse a name with new bytes,
		// and the latest user message is authoritative for this turn.
		_ = os.WriteFile(filepath.Join(dir, safeAttachmentName(a)), data, 0o644)
	}
}

// safeAttachmentName derives a filesystem-safe basename for an attachment,
// preferring the client-supplied name and falling back to the content hash.
func safeAttachmentName(a sessionstore.BlobRef) string {
	name := filepath.Base(strings.TrimSpace(a.Name))
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "/") {
		return a.Hash
	}
	return name
}
