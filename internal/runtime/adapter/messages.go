package adapter

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

type BlobLoader func(ctx context.Context, hash string) ([]byte, error)

type Reporter interface {
	Warn(msg string, kv ...any)
}

type Options struct {
	LoadBlob BlobLoader
	Report   Reporter
	ExtractDoc func(hash, mime string, data []byte) (string, bool)
	DropAttachments bool
}

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


const maxInlinedTextBytes = 256 << 10


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
