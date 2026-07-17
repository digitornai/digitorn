package sessionstore

import "strings"

func NormalizeMessageParts(m *MessagePayload) (parts []MessagePart, content string, toolCallIDs []string, attachments []BlobRef) {
	if m == nil {
		return nil, "", nil, nil
	}

	if len(m.Parts) > 0 {
		parts = m.Parts
		content, toolCallIDs, attachments = deriveLegacyFromParts(parts)
		return parts, content, toolCallIDs, attachments
	}

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
	return parts, m.Content, m.ToolCallIDs, append([]BlobRef(nil), m.Attachments...)
}

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
