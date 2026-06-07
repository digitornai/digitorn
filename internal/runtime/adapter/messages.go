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

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
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
			continue // a tool message already pulled forward next to its assistant
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
			// No result anywhere in the remaining stream. Synthesize an
			// interrupted result only if the conversation continued past this
			// assistant ; otherwise it's the pre-dispatch tail — leave it alone.
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

// findUnconsumedResult returns the index of the first not-yet-consumed tool
// message answering toolCallID at or after `from`, or -1 if none exists.
func findUnconsumedResult(msgs []llm.ChatMessage, consumed []bool, toolCallID string, from int) int {
	for j := from; j < len(msgs); j++ {
		if !consumed[j] && msgs[j].Role == "tool" && msgs[j].ToolCallID == toolCallID {
			return j
		}
	}
	return -1
}

// conversationContinuesAfter reports whether any message after index i is part
// of the ongoing conversation rather than just this assistant's own (pending)
// tool results — i.e. whether the turn moved on. A tool message answering one of
// this assistant's tool_call_ids doesn't count as "continuation" ; anything else
// does.
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

// convertOne handles ONE session-store Message. It can produce MULTIPLE
// llm.ChatMessage entries — specifically for "tool" messages carrying
// many ToolResultSpec parts, where each result becomes its own tool
// message (this is the shape OpenAI requires).
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

// convertConversational handles user / assistant / system roles. The
// Parts list becomes either a multi-part ChatMessage.Parts, or — when
// it's pure text — collapses back into ChatMessage.Content so providers
// that only accept strings (legacy Anthropic system prompts, etc.) work
// without further translation.
func convertConversational(ctx context.Context, m sessionstore.Message, opts Options) llm.ChatMessage {
	parts := effectiveParts(m)
	cm := llm.ChatMessage{Role: m.Role}

	// Replay the assistant's thinking-mode trace to the provider. Reasoning
	// models (DeepSeek thinking mode, xAI) require the prior reasoning_content
	// on assistant messages or they reject the request ; the worker maps this
	// onto the provider's reasoning_content field. Other providers ignore it.
	if m.Reasoning != "" {
		cm.ReasoningContent = m.Reasoning
	}

	// Collect tool calls separately ; they live on a dedicated field, not in Parts.
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
			// Shouldn't happen on user/assistant/system role — only "tool".
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
		// Single-part text or empty : collapse into Content so simple
		// providers don't need to parse multi-part.
		cm.Content = textOnlyBuf
	default:
		cm.Parts = contentParts
	}
	return cm
}

// convertToolMessage explodes a "tool" role Message into one ChatMessage
// per ToolResult inside it. OpenAI requires one tool message per
// tool_call_id ; Anthropic accepts grouped but we normalise to one-each
// for portability.
func convertToolMessage(ctx context.Context, m sessionstore.Message, opts Options) []llm.ChatMessage {
	parts := effectiveParts(m)
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
		// Result parts can themselves be multi-part (text + image).
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
			// Errors are conveyed as text. Some providers (OpenAI) also accept
			// an `is_error` flag — bifrost can apply that at serialise time.
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

// effectiveParts returns the message's Parts if set, otherwise
// synthesises a single text part from the legacy Content + the
// legacy Attachments — handles old events written before FT-1.
func effectiveParts(m sessionstore.Message) []sessionstore.MessagePart {
	if len(m.Parts) > 0 {
		return m.Parts
	}
	var parts []sessionstore.MessagePart
	if m.Content != "" {
		parts = append(parts, sessionstore.MessagePart{
			Type: sessionstore.PartTypeText,
			Text: m.Content,
		})
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

// legacyPartTypeFromMime mirrors the helper in sessionstore.parts.go,
// duplicated here so we don't introduce a circular dep on a private
// helper. Tiny enough that the duplication is acceptable.
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

// loadBinaryPart fetches the blob bytes for an image/audio/video/file
// part and returns the corresponding llm.ContentPart. Returns
// (zero, false) if the blob can't be loaded — the caller skips it
// and the failure is reported.
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
	return llm.ContentPart{
		Type: contentTypeFor(p.Type),
		Mime: p.Blob.Mime,
		Data: data,
	}, true
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

// PrependSystemPrompt returns a copy of msgs with a system message
// inserted at position 0 (or replacing an existing system at 0).
// Pure function ; multipart-safe (the prompt is added as Content, the
// downstream multi-part messages stay intact).
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
