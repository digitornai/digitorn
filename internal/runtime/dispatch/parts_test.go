package dispatch

import (
	"context"
	"io"
	"testing"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

type fakePutter struct {
	got  []byte
	mime string
}

func (f *fakePutter) Put(_ context.Context, mime string, r io.Reader) (sessionstore.BlobRef, error) {
	b, _ := io.ReadAll(r)
	f.got, f.mime = b, mime
	return sessionstore.BlobRef{Hash: "deadbeefhash", Mime: mime, Size: int64(len(b))}, nil
}

// An image OutputPart must be stored in the BlobStore and emitted as an image
// MessagePart carrying the BlobRef — the part the multipart adapter turns into
// vision content the model sees.
func TestPartsFromResult_ImageStoredAsBlobPart(t *testing.T) {
	fp := &fakePutter{}
	a := &BusAdapter{Blobs: fp}
	res := tool.Result{
		OutputParts: []tool.OutputPart{
			{Kind: tool.OutputText, Text: "[image pic.png]"},
			{Kind: tool.OutputImage, Bytes: []byte("PNGDATA"), Mime: "image/png", Name: "pic.png"},
		},
	}
	parts := a.partsFromResult(context.Background(), res)
	if len(parts) != 2 {
		t.Fatalf("want 2 parts (text + image), got %d: %+v", len(parts), parts)
	}
	if parts[0].Type != sessionstore.PartTypeText || parts[0].Text != "[image pic.png]" {
		t.Fatalf("part 0 should be the text note: %+v", parts[0])
	}
	if parts[1].Type != sessionstore.PartTypeImage || parts[1].Blob == nil {
		t.Fatalf("part 1 should be an image with a Blob ref: %+v", parts[1])
	}
	if parts[1].Blob.Hash != "deadbeefhash" || parts[1].Blob.Mime != "image/png" {
		t.Fatalf("blob ref wrong: %+v", parts[1].Blob)
	}
	if string(fp.got) != "PNGDATA" || fp.mime != "image/png" {
		t.Fatalf("bytes/mime not stored verbatim: got=%q mime=%q", fp.got, fp.mime)
	}
}

// With no BlobStore wired, an image degrades to a text note — never silently
// dropped (the model is told the bytes exist).
func TestPartsFromResult_ImageWithoutBlobStoreDegradesToText(t *testing.T) {
	a := &BusAdapter{} // Blobs nil
	res := tool.Result{OutputParts: []tool.OutputPart{
		{Kind: tool.OutputImage, Bytes: []byte("x"), Mime: "image/png", Name: "p.png"},
	}}
	parts := a.partsFromResult(context.Background(), res)
	if len(parts) != 1 || parts[0].Type != sessionstore.PartTypeText {
		t.Fatalf("want a single text fallback part, got %+v", parts)
	}
}

// No OutputParts → legacy single text part from Data (back-compat).
func TestPartsFromResult_LegacyTextFromData(t *testing.T) {
	a := &BusAdapter{}
	parts := a.partsFromResult(context.Background(), tool.Result{Data: "hello"})
	if len(parts) != 1 || parts[0].Type != sessionstore.PartTypeText || parts[0].Text != "hello" {
		t.Fatalf("legacy text path broken: %+v", parts)
	}
}
