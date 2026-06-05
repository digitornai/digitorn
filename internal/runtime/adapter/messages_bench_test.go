package adapter_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/adapter"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// buildTextHistory returns n messages alternating user / assistant with
// interleaved tool_call + tool_result pairs, so the benchmark exercises
// convertOne AND repairToolPairing on a realistic agent transcript.
func buildTextHistory(n int) []sessionstore.Message {
	msgs := make([]sessionstore.Message, 0, n)
	for i := 1; i <= n; i++ {
		switch i % 4 {
		case 1:
			msgs = append(msgs, sessionstore.Message{Seq: uint64(i), Role: "user",
				Content: fmt.Sprintf("user message %d with a realistic length of content here", i)})
		case 2:
			msgs = append(msgs, sessionstore.Message{Seq: uint64(i), Role: "assistant",
				Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeToolCall,
					ToolCall: &sessionstore.ToolCallSpec{ID: fmt.Sprintf("call-%d", i), Name: "read",
						Args: map[string]any{"path": "/some/file"}}}}})
		case 3:
			msgs = append(msgs, sessionstore.Message{Seq: uint64(i), Role: "tool",
				Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeToolResult,
					ToolResult: &sessionstore.ToolResultSpec{ToolCallID: fmt.Sprintf("call-%d", i-1),
						Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "tool output line"}}}}}})
		default:
			msgs = append(msgs, sessionstore.Message{Seq: uint64(i), Role: "assistant",
				Content: fmt.Sprintf("assistant reply %d", i)})
		}
	}
	return msgs
}

func BenchmarkMessagesToLLM_TextOnly(b *testing.B) {
	for _, n := range []int{50, 200, 1000} {
		hist := buildTextHistory(n)
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = adapter.MessagesToLLM(context.Background(), hist, adapter.Options{})
			}
		})
	}
}

// countingLoader records every blob fetch — the number the incremental
// converter (Finding #3) must drive toward zero on re-reads.
type countingLoader struct{ n atomic.Int64 }

func (c *countingLoader) load(context.Context, string) ([]byte, error) {
	c.n.Add(1)
	return []byte{0x1, 0x2, 0x3, 0x4}, nil
}

func buildImageHistory(numImages int) []sessionstore.Message {
	msgs := []sessionstore.Message{{Seq: 1, Role: "user", Content: "look at these"}}
	for i := 0; i < numImages; i++ {
		msgs = append(msgs, sessionstore.Message{Seq: uint64(i + 2), Role: "user",
			Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeImage,
				Blob: &sessionstore.BlobRef{Hash: fmt.Sprintf("img-%d", i), Mime: "image/png", Size: 4}}}})
	}
	return msgs
}

// TestMessagesToLLM_RefetchesBlobsEachCall pins the CURRENT behavior:
// MessagesToLLM is stateless, so every call re-loads every blob. With k
// LLM iterations in one turn that is k× redundant blob I/O — exactly what
// the incremental converter will remove. Documenting it here gives the
// optimization a known baseline to beat.
func TestMessagesToLLM_RefetchesBlobsEachCall(t *testing.T) {
	const numImages = 5
	hist := buildImageHistory(numImages)
	cl := &countingLoader{}
	opts := adapter.Options{LoadBlob: cl.load}

	for iter := 0; iter < 3; iter++ {
		_ = adapter.MessagesToLLM(context.Background(), hist, opts)
	}
	if got := cl.n.Load(); got != int64(3*numImages) {
		t.Fatalf("blob loads = %d, want %d (3 calls × %d images, stateless re-fetch)",
			got, 3*numImages, numImages)
	}
}

func BenchmarkMessagesToLLM_WithImages(b *testing.B) {
	const numImages = 10
	hist := buildImageHistory(numImages)
	cl := &countingLoader{}
	opts := adapter.Options{LoadBlob: cl.load}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = adapter.MessagesToLLM(context.Background(), hist, opts)
	}
	b.ReportMetric(float64(cl.n.Load())/float64(b.N), "blobloads/op")
}
