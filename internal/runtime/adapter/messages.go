// Package adapter converts between session-store types and LLM client
// types. Pure functions where possible ; the only I/O lives in
// blob fetching when a message contains binary attachments.
//
// The "MessagesToLLM" path is LOSSLESS by contract :
//
//   - Every sessionstore.MessagePart of type text/image/audio/video/file
//     becomes an llm.ContentPart of the same kind.
//   - Inline blob bytes are loaded via the supplied BlobLoader so the
//     worker stays oblivious to our blob store (it only ever sees
//     bytes on the wire).
//   - Tool-call parts inside an assistant message become entries in
//     llm.ChatMessage.ToolCalls (matching the OpenAI / Anthropic shape).
//   - Tool-result parts inside a "tool" message become a separate
//     ChatMessage with Role="tool", carrying the matching ToolCallID.
//   - Unknown part types are SKIPPED rather than failing the request —
//     we'd rather a slightly incomplete prompt than a broken turn.
//     The skip is logged via the optional Reporter so it's visible.
package adapter

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// BlobLoader fetches the bytes for a referenced blob. The runtime passes
// the BlobStore.Get adapter ; tests pass a stub. Returning empty bytes +
// nil is treated as "blob missing" — the adapter skips the part and
// reports it via Reporter.
type BlobLoader func(ctx context.Context, hash string) ([]byte, error)

// Reporter is the optional sink for adapter-level diagnostics : skipped
// parts, missing blobs, type-mismatches. Pass nil to discard. Used in
// production with slog ; in tests with a slice for assertions.
type Reporter interface {
	Warn(msg string, kv ...any)
}

// Options bundles the dependencies MessagesToLLM needs. All optional :
// passing the zero value disables blob loading (binary parts become
// no-ops) and silences diagnostics.
type Options struct {
	LoadBlob BlobLoader
	Report   Reporter
	// ExtractDoc, when set, turns a binary DOCUMENT blob (PDF, DOCX, …) into
	// plain text so it reaches every model as readable content rather than an
	// opaque file block. Keyed by content hash so the impl can cache. Returns
	// ok=false for anything it can't extract — the part then keeps its native
	// (file) block. Optional: nil disables document extraction.
	ExtractDoc func(hash, mime string, data []byte) (string, bool)
	// DropAttachments omits the user message's binary attachment parts from the
	// LLM prompt. The engine sets it when the app has a workdir + filesystem
	// `read` tool: attachments are materialised on disk and the agent reads them
	// on demand (read→vision), so inlining them here would duplicate (expensive)
	// image blocks every turn. When false (no read tool), attachments are inlined
	// so a model without file tools still sees them.
	DropAttachments bool
}

// MessagesToLLM converts the projected session-store message list into
// the LLM-compatible format. Chronological order is preserved.
//
// This function is the SINGLE BOUNDARY between persisted state and the
// LLM wire. Every byte that ends up in the prompt flows through here.
func MessagesToLLM(ctx context.Context, msgs []sessionstore.Message, opts Options) []llm.ChatMessage {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]llm.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		converted := convertOne(ctx, m, opts)
		out = append(out, converted...)
	}
	return repairToolPairing(out, opts)
}

// repairToolPairing guarantees the contract every chat provider enforces : an
// assistant message carrying tool_calls MUST be IMMEDIATELY followed by a tool
// message for EACH tool_call_id, with nothing in between.
//
// Two ways that invariant gets broken in persisted history :
//
//   - A MISPLACED result : the approval flow (and message queueing) interleaves
//     a message between an assistant's tool_call and its result — the user's
//     "approve" reply is persisted as a user_message that lands BEFORE the
//     gated tool runs, so the stream reads [assistant(tool_calls), user, tool].
//     OpenAI/Anthropic tolerate the gap ; DeepSeek rejects the whole request
//     ("Messages with role 'tool' must be a response to a preceding message
//     with 'tool_calls'"). We PULL every answering tool message forward to sit
//     directly after its assistant, wherever it sits in the stream ; the
//     interleaved messages keep their order and fall in right after the tool
//     block.
//
//   - A MISSING result : a turn aborted (or the daemon crashed) WHILE a tool was
//     executing leaves the assistant's tool_call durable but its result absent.
//     We synthesize a terminal "interrupted" result so one bad turn can't poison
//     every future request — but ONLY when the conversation actually moved on
//     past this assistant. A trailing assistant+tool_calls with no results yet
//     is the normal pre-dispatch state, not a gap.
//
// This is the LAST line of defense at the single state→wire boundary, so it
// holds no matter how the gap arose.
func repairToolPairing(msgs []llm.ChatMessage, opts Options) []llm.ChatMessage {
	consumed := make([]bool, len(msgs))
	out := make([]llm.ChatMessage, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		if consumed[i] {
			continue 
		}
		m := msgs[i]
		out = append(out, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		idSet := make(map[string]bool, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				idSet[tc.ID] = true
			}
		}
		moved := conversationContinuesAfter(msgs, consumed, idSet, i)
		for _, tc := range m.ToolCalls {
			if tc.ID == "" {
				continue
			}
			if found := findUnconsumedResult(msgs, consumed, tc.ID, i+1); found >= 0 {
				out = append(out, msgs[found])
				consumed[found] = true
				continue
			}
			if moved {
				warn(opts, "adapter: synthesizing interrupted result for unanswered tool_call",
					"tool_call_id", tc.ID, "name", tc.Name)
				out = append(out, llm.ChatMessage{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    "[Tool interrupted: it was stopped before returning — NO result was produced and nothing was applied. Re-run this tool if its output is still needed.]",
				})
			}
		}
	}
	return out
}


func findUnconsumedResult(msgs []llm.ChatMessage, consumed []bool, toolCallID string, from int) int {
	for j := from; j < len(msgs); j++ {
		if !consumed[j] && msgs[j].Role == "tool" && msgs[j].ToolCallID == toolCallID {
			return j
		}
	}
	return -1
}


func conversationContinuesAfter(msgs []llm.ChatMessage, consumed []bool, idSet map[string]bool, i int) bool {
	for j := i + 1; j < len(msgs); j++ {
		if consumed[j] {
			continue
		}
		if msgs[j].Role == "tool" && idSet[msgs[j].ToolCallID] {
			continue
		}
		return true
	}
	return false
}


func convertOne(ctx context.Context, m sessionstore.Message, opts Options) []llm.ChatMessage {
	switch m.Role {
	case "user", "assistant", "system":
		return []llm.ChatMessage{convertConversational(ctx, m, opts)}
	case "tool":
		return convertToolMessage(ctx, m, opts)
	default:
		warn(opts, "adapter: dropping unknown role", "role", m.Role)
		return nil
	}
}


func convertConversational(ctx context.Context, m sessionstore.Message, opts Options) llm.ChatMessage {
	parts := effectiveParts(m, opts.DropAttachments)
	cm := llm.ChatMessage{Role: m.Role}


	if m.Reasoning != "" {
		cm.ReasoningContent = m.Reasoning
	}


	var toolCalls []llm.ChatToolCall
	var contentParts []llm.ContentPart
	var textOnlyBuf string
	pureText := true

	for _, p := range parts {
		switch p.Type {
		case sessionstore.PartTypeText:
			if p.Text == "" {
				continue
			}
			textOnlyBuf = appendText(textOnlyBuf, p.Text)
			contentParts = append(contentParts, llm.ContentPart{
				Type: llm.ContentTypeText,
				Text: p.Text,
			})
		case sessionstore.PartTypeImage, sessionstore.PartTypeAudio,
			sessionstore.PartTypeVideo, sessionstore.PartTypeFile:
			pureText = false
			cp, ok := loadBinaryPart(ctx, p, opts)
			if !ok {
				continue
			}
			contentParts = append(contentParts, cp)
		case sessionstore.PartTypeToolCall:
			if p.ToolCall == nil || p.ToolCall.ID == "" {
				continue
			}
			toolCalls = append(toolCalls, llm.ChatToolCall{
				ID:        p.ToolCall.ID,
				Type:      "function",
				Name:      p.ToolCall.Name,
				Arguments: p.ToolCall.Args,
			})
		case sessionstore.PartTypeToolResult:

			warn(opts, "adapter: tool_result in non-tool message, skipped",
				"role", m.Role)
		default:
			warn(opts, "adapter: skipping unknown part type", "type", p.Type)
		}
	}

	if len(toolCalls) > 0 {
		cm.ToolCalls = toolCalls
	}
	switch {
	case pureText && len(contentParts) <= 1:

		cm.Content = textOnlyBuf
	default:
		cm.Parts = contentParts
	}
	return cm
}


func convertToolMessage(ctx context.Context, m sessionstore.Message, opts Options) []llm.ChatMessage {
	parts := effectiveParts(m, opts.DropAttachments)
	var out []llm.ChatMessage
	for _, p := range parts {
		if p.Type != sessionstore.PartTypeToolResult || p.ToolResult == nil {
			warn(opts, "adapter: non-result part in tool message, skipped",
				"type", p.Type)
			continue
		}
		tr := p.ToolResult
		cm := llm.ChatMessage{
			Role:       "tool",
			ToolCallID: tr.ToolCallID,
		}

		var resultParts []llm.ContentPart
		var textOnlyBuf string
		pureText := true
		for _, rp := range tr.Parts {
			switch rp.Type {
			case sessionstore.PartTypeText:
				if rp.Text == "" {
					continue
				}
				textOnlyBuf = appendText(textOnlyBuf, rp.Text)
				resultParts = append(resultParts, llm.ContentPart{
					Type: llm.ContentTypeText,
					Text: rp.Text,
				})
			case sessionstore.PartTypeImage, sessionstore.PartTypeAudio,
				sessionstore.PartTypeVideo, sessionstore.PartTypeFile:
				pureText = false
				cp, ok := loadBinaryPart(ctx, rp, opts)
				if !ok {
					continue
				}
				resultParts = append(resultParts, cp)
			default:
				warn(opts, "adapter: skipping unknown nested part type in tool result",
					"type", rp.Type)
			}
		}
		if tr.Error != "" {

			errPrefix := fmt.Sprintf("[tool_error] %s", tr.Error)
			textOnlyBuf = appendText(textOnlyBuf, errPrefix)
			resultParts = append(resultParts, llm.ContentPart{
				Type: llm.ContentTypeText,
				Text: errPrefix,
			})
		}
		if pureText && len(resultParts) <= 1 {
			cm.Content = textOnlyBuf
		} else {
			cm.Parts = resultParts
		}
		out = append(out, cm)
	}
	return out
}


func effectiveParts(m sessionstore.Message, dropAttachments bool) []sessionstore.MessagePart {
	if len(m.Parts) > 0 {
		if !dropAttachments {
			return m.Parts
		}
		// Materialised-to-workdir mode: drop binary (blob) parts — the agent
		// reads them on demand — but keep text / tool parts.
		kept := make([]sessionstore.MessagePart, 0, len(m.Parts))
		for _, p := range m.Parts {
			if p.Blob != nil {
				continue
			}
			kept = append(kept, p)
		}
		return kept
	}
	var parts []sessionstore.MessagePart
	if m.Content != "" {
		parts = append(parts, sessionstore.MessagePart{
			Type: sessionstore.PartTypeText,
			Text: m.Content,
		})
	}
	if dropAttachments {
		return parts
	}
	for i := range m.Attachments {
		att := m.Attachments[i]
		parts = append(parts, sessionstore.MessagePart{
			Type: legacyPartTypeFromMime(att.Mime),
			Blob: &att,
		})
	}
	return parts
}


func legacyPartTypeFromMime(mime string) string {
	switch {
	case len(mime) >= 6 && mime[:6] == "image/":
		return sessionstore.PartTypeImage
	case len(mime) >= 6 && mime[:6] == "audio/":
		return sessionstore.PartTypeAudio
	case len(mime) >= 6 && mime[:6] == "video/":
		return sessionstore.PartTypeVideo
	default:
		return sessionstore.PartTypeFile
	}
}


func loadBinaryPart(ctx context.Context, p sessionstore.MessagePart, opts Options) (llm.ContentPart, bool) {
	if p.Blob == nil {
		warn(opts, "adapter: binary part missing blob ref", "type", p.Type)
		return llm.ContentPart{}, false
	}
	if opts.LoadBlob == nil {
		warn(opts, "adapter: no BlobLoader, dropping binary part",
			"type", p.Type, "hash", p.Blob.Hash)
		return llm.ContentPart{}, false
	}
	data, err := opts.LoadBlob(ctx, p.Blob.Hash)
	if err != nil {
		warn(opts, "adapter: blob load failed",
			"hash", p.Blob.Hash, "err", err.Error())
		return llm.ContentPart{}, false
	}
	if len(data) == 0 {
		warn(opts, "adapter: blob is empty", "hash", p.Blob.Hash)
		return llm.ContentPart{}, false
	}

	if isTextualMime(p.Blob.Mime) && utf8.Valid(data) {
		return llm.ContentPart{
			Type: llm.ContentTypeText,
			Text: inlineTextAttachment(p.Blob.Mime, data),
		}, true
	}

	if opts.ExtractDoc != nil {
		if txt, ok := opts.ExtractDoc(p.Blob.Hash, p.Blob.Mime, data); ok {
			return llm.ContentPart{
				Type: llm.ContentTypeText,
				Text: inlineTextAttachment(p.Blob.Mime, []byte(txt)),
			}, true
		}
	}
	return llm.ContentPart{
		Type: contentTypeFor(p.Type),
		Mime: p.Blob.Mime,
		Data: data,
	}, true
}


const maxInlinedTextBytes = 256 << 10 // 256 KiB


func isTextualMime(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	switch mime {
	case "application/json", "application/ld+json", "application/xml",
		"application/xhtml+xml", "application/yaml", "application/x-yaml",
		"application/csv", "application/javascript", "application/ecmascript",
		"application/x-ndjson", "application/toml", "application/x-sh",
		"application/sql", "application/graphql", "application/x-www-form-urlencoded":
		return true
	}
	return false
}


func inlineTextAttachment(mime string, data []byte) string {
	truncated := false
	if len(data) > maxInlinedTextBytes {
		data = data[:maxInlinedTextBytes]
		for len(data) > 0 && !utf8.Valid(data) { 
			data = data[:len(data)-1]
		}
		truncated = true
	}
	var b strings.Builder
	b.WriteString("[Attached document")
	if m := strings.TrimSpace(mime); m != "" {
		b.WriteString(" (")
		b.WriteString(m)
		b.WriteByte(')')
	}
	b.WriteString("]\n")
	b.Write(data)
	if truncated {
		b.WriteString("\n[… document truncated …]")
	}
	return b.String()
}

func contentTypeFor(partType string) string {
	switch partType {
	case sessionstore.PartTypeImage:
		return llm.ContentTypeImage
	case sessionstore.PartTypeAudio:
		return llm.ContentTypeAudio
	case sessionstore.PartTypeVideo:
		return llm.ContentTypeVideo
	default:
		return llm.ContentTypeFile
	}
}

func appendText(buf, s string) string {
	if buf == "" {
		return s
	}
	return buf + "\n" + s
}

func warn(opts Options, msg string, kv ...any) {
	if opts.Report != nil {
		opts.Report.Warn(msg, kv...)
	}
}


func PrependSystemPrompt(msgs []llm.ChatMessage, systemPrompt string) []llm.ChatMessage {
	if systemPrompt == "" {
		return msgs
	}
	if len(msgs) > 0 && msgs[0].Role == "system" {
		out := make([]llm.ChatMessage, len(msgs))
		copy(out, msgs)
		out[0].Content = systemPrompt
		out[0].Parts = nil
		return out
	}
	out := make([]llm.ChatMessage, 0, len(msgs)+1)
	out = append(out, llm.ChatMessage{Role: "system", Content: systemPrompt})
	out = append(out, msgs...)
	return out
}
