package sessionstore

import (
	"reflect"
	"testing"
)

// TestNormalizeMessageParts_NewWriter_KeepsPartsExactly asserts that a
// writer using the new Parts field gets back exactly the parts it
// supplied — no reordering, no loss, no synthesis.
func TestNormalizeMessageParts_NewWriter_KeepsPartsExactly(t *testing.T) {
	blob := BlobRef{Hash: "abc123", Mime: "image/png", Size: 1024}
	tc := &ToolCallSpec{ID: "call-1", Name: "search", Args: map[string]any{"q": "go"}}

	in := &MessagePayload{
		Role: "assistant",
		Parts: []MessagePart{
			{Type: PartTypeText, Text: "Here is the result :"},
			{Type: PartTypeImage, Blob: &blob},
			{Type: PartTypeToolCall, ToolCall: tc},
		},
	}

	parts, content, toolIDs, atts := NormalizeMessageParts(in)

	if !reflect.DeepEqual(parts, in.Parts) {
		t.Fatalf("parts mutated by NormalizeMessageParts\n  got  %+v\n  want %+v", parts, in.Parts)
	}
	if content != "Here is the result :" {
		t.Fatalf("derived Content wrong : %q", content)
	}
	if len(toolIDs) != 1 || toolIDs[0] != "call-1" {
		t.Fatalf("derived ToolCallIDs wrong : %v", toolIDs)
	}
	if len(atts) != 1 || atts[0].Hash != "abc123" {
		t.Fatalf("derived Attachments wrong : %+v", atts)
	}
}

// TestNormalizeMessageParts_LegacyWriter_SynthesizesParts asserts that
// a legacy writer (Content + Attachments only, no Parts) gets a Parts
// list synthesized for them — so new readers see a uniform shape.
func TestNormalizeMessageParts_LegacyWriter_SynthesizesParts(t *testing.T) {
	in := &MessagePayload{
		Role:    "user",
		Content: "hello",
		Attachments: []BlobRef{
			{Hash: "img1", Mime: "image/jpeg", Size: 4096},
			{Hash: "aud1", Mime: "audio/mp3", Size: 2048},
			{Hash: "doc1", Mime: "application/pdf", Size: 8192},
		},
	}

	parts, content, _, atts := NormalizeMessageParts(in)

	if content != "hello" {
		t.Fatalf("legacy Content lost : %q", content)
	}
	if len(parts) != 4 {
		t.Fatalf("expected 4 parts (1 text + 3 blobs), got %d : %+v", len(parts), parts)
	}
	if parts[0].Type != PartTypeText || parts[0].Text != "hello" {
		t.Fatalf("synthesized text part wrong : %+v", parts[0])
	}
	if parts[1].Type != PartTypeImage || parts[1].Blob == nil {
		t.Fatalf("synthesized image part wrong : %+v", parts[1])
	}
	if parts[2].Type != PartTypeAudio || parts[2].Blob == nil {
		t.Fatalf("synthesized audio part wrong : %+v", parts[2])
	}
	if parts[3].Type != PartTypeFile || parts[3].Blob == nil {
		t.Fatalf("synthesized file part wrong (PDF should be 'file') : %+v", parts[3])
	}
	if len(atts) != 3 {
		t.Fatalf("legacy Attachments preserved wrong : %v", atts)
	}
}

// TestNormalizeMessageParts_TextOnly_LossPath asserts the simplest path
// — pure text message — survives perfectly through normalization. This
// covers > 95% of real-world traffic.
func TestNormalizeMessageParts_TextOnly_LossPath(t *testing.T) {
	in := &MessagePayload{Role: "user", Content: "ping"}
	parts, content, _, _ := NormalizeMessageParts(in)
	if content != "ping" {
		t.Fatalf("text content lost : %q", content)
	}
	if len(parts) != 1 || parts[0].Type != PartTypeText || parts[0].Text != "ping" {
		t.Fatalf("text-only synthesis wrong : %+v", parts)
	}
}

// TestNormalizeMessageParts_MultipleTextParts_ConcatenatedInContent
// asserts the legacy Content field correctly captures all text parts
// (so legacy readers don't miss anything when the message is multi-part).
func TestNormalizeMessageParts_MultipleTextParts_ConcatenatedInContent(t *testing.T) {
	in := &MessagePayload{
		Role: "assistant",
		Parts: []MessagePart{
			{Type: PartTypeText, Text: "First line."},
			{Type: PartTypeText, Text: "Second line."},
		},
	}
	_, content, _, _ := NormalizeMessageParts(in)
	want := "First line.\nSecond line."
	if content != want {
		t.Fatalf("multi-text concat wrong\n  got  %q\n  want %q", content, want)
	}
}

// TestNormalizeMessageParts_NilSafe asserts NormalizeMessageParts handles
// a nil payload without panicking. Protects projection from bugs in
// writers that emit malformed events.
func TestNormalizeMessageParts_NilSafe(t *testing.T) {
	parts, content, toolIDs, atts := NormalizeMessageParts(nil)
	if parts != nil || content != "" || toolIDs != nil || atts != nil {
		t.Fatalf("nil input must yield zero values, got %+v %q %v %v", parts, content, toolIDs, atts)
	}
}

// TestNormalizeMessageParts_ForwardCompatUnknownType asserts that an
// unknown part type is preserved through normalization. Future formats
// (e.g. "embedding") survive round-trip even before we add explicit
// handling.
func TestNormalizeMessageParts_ForwardCompatUnknownType(t *testing.T) {
	in := &MessagePayload{
		Role: "user",
		Parts: []MessagePart{
			{Type: "embedding", Text: "[0.1,0.2,0.3]"},
		},
	}
	parts, _, _, _ := NormalizeMessageParts(in)
	if len(parts) != 1 || parts[0].Type != "embedding" {
		t.Fatalf("unknown part type dropped : %+v", parts)
	}
}

// TestProjectionAppliesNormalization is the integration check : an
// event going through projection produces a Message whose Parts +
// Content are normalized consistently.
func TestProjectionAppliesNormalization(t *testing.T) {
	st := NewSessionState("sess-1")
	blob := BlobRef{Hash: "img-h", Mime: "image/png", Size: 100}
	Apply(st, &Event{
		Seq:        1,
		Type:       EventUserMessage,
		SessionID:  "sess-1",
		TsUnixNano: 1,
		Message: &MessagePayload{
			Role: "user",
			Parts: []MessagePart{
				{Type: PartTypeText, Text: "what's in this image ?"},
				{Type: PartTypeImage, Blob: &blob},
			},
		},
	})
	if len(st.Messages) != 1 {
		t.Fatalf("expected 1 projected message, got %d", len(st.Messages))
	}
	got := st.Messages[0]
	if len(got.Parts) != 2 {
		t.Fatalf("projection lost parts : %+v", got)
	}
	if got.Content != "what's in this image ?" {
		t.Fatalf("projection didn't sync Content : %q", got.Content)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Hash != "img-h" {
		t.Fatalf("projection didn't sync Attachments : %+v", got.Attachments)
	}
}
