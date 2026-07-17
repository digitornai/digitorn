package adapter_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime/adapter"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

func anyBlobLoader(_ context.Context, hash string) ([]byte, error) {
	return []byte("blob:" + hash), nil
}

func converterCorpus() [][]sessionstore.Message {
	return [][]sessionstore.Message{
		nil,
		{{Seq: 1, Role: "user", Content: "hi"}},
		{
			{Seq: 1, Role: "system", Content: "be helpful"},
			{Seq: 2, Role: "user", Content: "2+2?"},
			{Seq: 3, Role: "assistant", Content: "4"},
		},
		{
			{Seq: 1, Role: "user", Content: "do X"},
			{Seq: 2, Role: "assistant", Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{ID: "c1", Name: "x"}}}},
			{Seq: 3, Role: "user", Content: "never mind"},
		},
		{
			{Seq: 1, Role: "assistant", Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{ID: "c1", Name: "read"}}}},
			{Seq: 2, Role: "tool", Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{
					ToolCallID: "c1", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}}}}}},
			{Seq: 3, Role: "user", Content: "next"},
		},
		{
			{Seq: 1, Role: "user", Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeText, Text: "look"},
				{Type: sessionstore.PartTypeImage, Blob: &sessionstore.BlobRef{Hash: "u-img", Mime: "image/png", Size: 8}}}},
			{Seq: 2, Role: "tool", Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{
					ToolCallID: "c9", Parts: []sessionstore.MessagePart{
						{Type: sessionstore.PartTypeText, Text: "shot"},
						{Type: sessionstore.PartTypeImage, Blob: &sessionstore.BlobRef{Hash: "t-img", Mime: "image/png", Size: 8}}}}}}},
		},
		{
			{Seq: 1, Role: "user", Content: "hi"},
			{Seq: 2, Role: "moderator", Content: "hidden"},
			{Seq: 3, Role: "", Content: "no role"},
			{Seq: 4, Role: "assistant", Content: "hello"},
		},
		{
			{Seq: 0, Role: "system", Content: "summary of earlier conversation"},
			{Seq: 7, Role: "user", Content: "recent question"},
			{Seq: 8, Role: "assistant", Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{ID: "c7", Name: "grep"}}}},
			{Seq: 9, Role: "tool", Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{
					ToolCallID: "c7", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "match"}}}}}},
			{Seq: 10, Role: "assistant", Content: "the answer"},
		},
		buildTextHistory(257),
		buildImageHistory(12),
	}
}

func TestConverter_IdenticalToMessagesToLLM(t *testing.T) {
	opts := adapter.Options{LoadBlob: anyBlobLoader}
	for idx, hist := range converterCorpus() {
		want := adapter.MessagesToLLM(context.Background(), hist, opts)

		gotFull := adapter.NewConverter(opts).Convert(context.Background(), hist)
		if !reflect.DeepEqual(gotFull, want) {
			t.Fatalf("corpus[%d] full-shot mismatch:\n got=%#v\nwant=%#v", idx, gotFull, want)
		}

		c := adapter.NewConverter(opts)
		var grown []llm.ChatMessage
		for k := 1; k <= len(hist); k++ {
			grown = c.Convert(context.Background(), hist[:k])
		}
		if len(hist) == 0 {
			grown = c.Convert(context.Background(), hist)
		}
		if !reflect.DeepEqual(grown, want) {
			t.Fatalf("corpus[%d] progressive-growth mismatch:\n got=%#v\nwant=%#v", idx, grown, want)
		}
	}
}

func BenchmarkConverter_Turn(b *testing.B) {
	base := buildTextHistory(200)
	const iterations = 10
	opts := adapter.Options{}

	b.Run("stateless_MessagesToLLM", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			hist := append([]sessionstore.Message(nil), base...)
			for it := 0; it < iterations; it++ {
				hist = append(hist, sessionstore.Message{
					Seq: uint64(len(hist) + 1), Role: "assistant", Content: fmt.Sprintf("step %d", it)})
				_ = adapter.MessagesToLLM(context.Background(), hist, opts)
			}
		}
	})

	b.Run("incremental_Converter", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			hist := append([]sessionstore.Message(nil), base...)
			c := adapter.NewConverter(opts)
			for it := 0; it < iterations; it++ {
				hist = append(hist, sessionstore.Message{
					Seq: uint64(len(hist) + 1), Role: "assistant", Content: fmt.Sprintf("step %d", it)})
				_ = c.Convert(context.Background(), hist)
			}
		}
	})
}
