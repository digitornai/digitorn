package sessionstore

import "strings"

// NormalizeMessageParts is the single source of truth for converting a
// persisted MessagePayload into a self-consistent projected Message.
//
// Two write paths feed this :
//
//  1. NEW writers fill Parts (multi-part native — text + images + tool
//     calls in one message). Legacy fields stay empty ; we derive
//     them here for back-compat readers.
//
//  2. LEGACY writers fill Content / Attachments / ToolCallIDs and
//     leave Parts nil. We synthesize Parts so new code can read a
//     uniform multi-part shape.
//
// Both paths produce the same four-tuple :
//   - parts        : []MessagePart (always non-nil if msg has content)
//   - content      : string concat of all "text" parts
//   - toolCallIDs  : []string of tool_call.ID for all "tool_call" parts
//   - attachments  : []BlobRef of all "image|audio|video|file" parts
//
// Empty inputs yield (nil, "", nil, nil) — never panics.
func NormalizeMessageParts(m *MessagePayload) (parts []MessagePart, content string, toolCallIDs []string, attachments []BlobRef) {
	if m == nil {
		return nil, "", nil, nil
	}

	// Path 1 : Parts provided. They ARE the truth, derive everything else.
	if len(m.Parts) > 0 {
		parts = m.Parts
		content, toolCallIDs, attachments = deriveLegacyFromParts(parts)
		return parts, content, toolCallIDs, attachments
	}

	// Path 2 : legacy fields only. Synthesize Parts.
	if m.Content != "" {
		parts = append(parts, MessagePart{Type: PartTypeText, Text: m.Content})
	}
	for i := range m.Attachments {
		att := m.Attachments[i]
		parts = append(parts, MessagePart{
			Type: partTypeFromMime(att.Mime),
			Blob: &att,
		})
	}
	// ToolCallIDs is legacy : just a reference list, no full args.
	// We can't reconstruct tool_call Parts from IDs alone — they live
	// inside their own ToolCallState. So leave them as ToolCallIDs only.
	return parts, m.Content, m.ToolCallIDs, append([]BlobRef(nil), m.Attachments...)
}

// deriveLegacyFromParts builds the back-compat fields from a Parts list.
// Text parts are concatenated with a single "\n" between them ; blobs
// produce attachments ; tool_call parts produce tool_call_ids.
func deriveLegacyFromParts(parts []MessagePart) (content string, toolCallIDs []string, attachments []BlobRef) {
	var textBits []string
	for _, p := range parts {
		switch p.Type {
		case PartTypeText:
			if p.Text != "" {
				textBits = append(textBits, p.Text)
			}
		case PartTypeImage, PartTypeAudio, PartTypeVideo, PartTypeFile:
			if p.Blob != nil {
				attachments = append(attachments, *p.Blob)
			}
		case PartTypeToolCall:
			if p.ToolCall != nil && p.ToolCall.ID != "" {
				toolCallIDs = append(toolCallIDs, p.ToolCall.ID)
			}
		}
	}
	content = strings.Join(textBits, "\n")
	return content, toolCallIDs, attachments
}

// partTypeFromMime picks the right discriminator from a MIME type when
// synthesizing Parts from a legacy Attachments list. Falls back to the
// generic "file" type for anything we don't recognize.
func partTypeFromMime(mime string) string {
	mime = strings.ToLower(mime)
	switch {
	case strings.HasPrefix(mime, "image/"):
		return PartTypeImage
	case strings.HasPrefix(mime, "audio/"):
		return PartTypeAudio
	case strings.HasPrefix(mime, "video/"):
		return PartTypeVideo
	default:
		return PartTypeFile
	}
}
