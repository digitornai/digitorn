package tool

import (
	"context"
	"encoding/json"
)

// Handler is the function signature every tool must implement.
// The context carries execution metadata (timeouts, cancellation, agent context).
// Params is a typed parameters object (unmarshaled from JSON by the dispatcher).
type Handler func(ctx context.Context, params json.RawMessage) (Result, error)

// Result is the outcome of a tool execution.
type Result struct {
	Success  bool           `json:"success"`
	Data     any            `json:"data,omitempty"`
	Error    string         `json:"error,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Display  *DisplayHint   `json:"display,omitempty"`
	// Diff, when set, is a CLIENT-ONLY render payload for a file mutation
	// (edit/write). It is forwarded to the UI verbatim so it can draw the
	// insertions/deletions, and is NEVER shown to the LLM — the dispatch layer
	// puts only the text Data/Parts in the model's context. Metadata likewise
	// rides to the client and is invisible to the model.
	Diff *DiffView `json:"-"`

	// OutputParts lets a tool return RICH, multi-format output the model can
	// actually consume — most importantly an image (read of a PNG/JPG → the
	// vision model SEES it) or a binary file, alongside text. When set, the
	// dispatch layer stores any binary bytes in the content-addressed BlobStore
	// and forwards image/file parts to the LLM via the multipart adapter; when
	// empty, the layer falls back to the text Data. In-process only (raw Bytes
	// are not JSON-serialised across the worker boundary).
	OutputParts []OutputPart `json:"-"`
}

// OutputPart kinds for Result.OutputParts.
const (
	OutputText  = "text"
	OutputImage = "image"
	OutputAudio = "audio"
	OutputVideo = "video"
	OutputFile  = "file"
)

// OutputPart is one piece of a tool's rich output : either text, or a binary
// blob (image/audio/video/file) carried as raw Bytes + its MIME type. The
// dispatch layer turns image/file parts into LLM-visible content via the
// BlobStore + multipart adapter.
type OutputPart struct {
	Kind  string `json:"kind"`
	Text  string `json:"text,omitempty"`
	Bytes []byte `json:"-"`
	Mime  string `json:"mime,omitempty"`
	Name  string `json:"name,omitempty"`
}

// DiffView is the client-facing diff of a file mutation. Field JSON tags match
// the legacy daemon's tool-result wire shape so existing clients render it with
// no change : a parseable unified diff, a short human summary, and the (capped)
// before/after content.
type DiffView struct {
	Unified         string `json:"unified_diff,omitempty"`
	Summary         string `json:"diff,omitempty"`
	PreviousContent string `json:"previous_content,omitempty"`
	NewContent      string `json:"new_content,omitempty"`
	Additions       int    `json:"additions,omitempty"`
	Deletions       int    `json:"deletions,omitempty"`
}

// DisplayHint suggests how the UI should render the result.
type DisplayHint struct {
	Type    string `json:"type"` // "text", "code", "markdown", "json", "table", "image"
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
}
