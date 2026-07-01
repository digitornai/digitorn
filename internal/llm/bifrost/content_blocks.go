package bifrost

import (
	"encoding/base64"
	"fmt"

	schemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/digitornai/digitorn/internal/llm"
)

// buildContentBlocks translates a daemon-side ChatMessage into Bifrost's
// typed ContentBlock array. Required whenever cache hints are present
// (Anthropic reads cache_control off blocks, not strings) or when the
// caller supplied multi-modal Parts.
//
// Strategy :
//
//  1. If Parts is non-empty, each Part becomes one block ;
//     CacheControl is forwarded per-block.
//  2. Otherwise, Content (string) becomes ONE text block carrying the
//     message-level CacheControl.
//
// We always emit at least one block — never an empty array, which would
// fail OpenAI-compat strict validators downstream.
func buildContentBlocks(m *llm.ChatMessage) []schemas.ChatContentBlock {
	// Path A — multimodal: one block per Part.
	if len(m.Parts) > 0 {
		out := make([]schemas.ChatContentBlock, 0, len(m.Parts))
		for i := range m.Parts {
			out = append(out, partToBlock(&m.Parts[i]))
		}
		return out
	}
	// Path B — string content: single text block, optionally marked.
	text := m.Content // copy: we'll take its address
	blk := schemas.ChatContentBlock{
		Type: schemas.ChatContentBlockTypeText,
		Text: &text,
	}
	if m.CacheControl != nil {
		blk.CacheControl = toBifrostCacheControl(m.CacheControl)
	}
	return []schemas.ChatContentBlock{blk}
}

// partToBlock converts one llm.ContentPart into a Bifrost ContentBlock.
// Each ContentPart Type maps to a Bifrost block kind ; binary parts get
// wrapped into the matching image/audio/file struct with a base64 data
// URL so providers that read URLs (most) can consume them inline.
func partToBlock(p *llm.ContentPart) schemas.ChatContentBlock {
	out := schemas.ChatContentBlock{}
	if p.CacheControl != nil {
		out.CacheControl = toBifrostCacheControl(p.CacheControl)
	}

	switch p.Type {
	case llm.ContentTypeText, "":
		out.Type = schemas.ChatContentBlockTypeText
		t := p.Text
		out.Text = &t

	case llm.ContentTypeImage:
		out.Type = schemas.ChatContentBlockTypeImage
		out.ImageURLStruct = &schemas.ChatInputImage{
			URL: dataOrURL(p, "image"),
		}

	case llm.ContentTypeAudio:
		out.Type = schemas.ChatContentBlockTypeInputAudio
		fmt := audioFormat(p.Mime)
		out.InputAudio = &schemas.ChatInputAudio{
			Data:   dataB64(p.Data),
			Format: &fmt,
		}

	case llm.ContentTypeFile:
		out.Type = schemas.ChatContentBlockTypeFile
		out.File = &schemas.ChatInputFile{
			FileData: ptrStr(dataOrURL(p, "file")),
		}

	default:
		// Unknown type → fallback to text with a debug marker so the
		// upstream provider sees SOMETHING rather than a malformed block.
		out.Type = schemas.ChatContentBlockTypeText
		s := fmt.Sprintf("[unsupported content type: %q]", p.Type)
		out.Text = &s
	}
	return out
}

// toBifrostCacheControl converts our local marker to Bifrost's. Both
// structs are tiny (one field), so this is a one-shot copy.
func toBifrostCacheControl(cc *llm.CacheControl) *schemas.CacheControl {
	if cc == nil {
		return nil
	}
	return &schemas.CacheControl{
		Type: schemas.CacheControlType(cc.Type),
	}
}

// dataOrURL picks the URL when set, otherwise builds a base64 data URI
// from inline bytes. Used for image / file blocks.
func dataOrURL(p *llm.ContentPart, kind string) string {
	if p.URL != "" {
		return p.URL
	}
	if len(p.Data) == 0 {
		return ""
	}
	mime := p.Mime
	if mime == "" {
		// Sensible defaults so providers don't reject the block on
		// missing mime — they sniff the magic bytes anyway.
		switch kind {
		case "image":
			mime = "image/png"
		default:
			mime = "application/octet-stream"
		}
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
}

func dataB64(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// audioFormat maps audio MIME to Bifrost's compact format identifier.
// Anything else falls through as "wav" (the most lenient option that
// providers accept by default).
func audioFormat(mime string) string {
	switch mime {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/wav", "audio/x-wav":
		return "wav"
	case "audio/ogg":
		return "opus"
	case "audio/flac":
		return "flac"
	}
	return "wav"
}

func ptrStr(s string) *string { return &s }
