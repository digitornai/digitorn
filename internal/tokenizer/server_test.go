package tokenizer

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/tokencount"
)

func TestServer_CountMatchesCounter(t *testing.T) {
	ref := tokencount.New()
	srv := NewServer(tokencount.New())
	texts := []string{"hello world", "a longer message with several tokens in it", ""}

	resp, err := srv.Count(context.Background(), &CountRequest{Texts: texts, Provider: "openai", Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if len(resp.Counts) != len(texts) {
		t.Fatalf("got %d counts for %d texts", len(resp.Counts), len(texts))
	}
	total := 0
	for i, txt := range texts {
		want := ref.Count(txt, "openai", "gpt-4o")
		if resp.Counts[i] != want {
			t.Errorf("text[%d] count=%d, want %d", i, resp.Counts[i], want)
		}
		total += want
	}
	if resp.Total != total {
		t.Errorf("total=%d, want %d", resp.Total, total)
	}
	if resp.Counts[2] != 0 {
		t.Errorf("empty text must be 0, got %d", resp.Counts[2])
	}
}

func TestServer_EmptyBatch(t *testing.T) {
	resp, err := NewServer(nil).Count(context.Background(), &CountRequest{Provider: "openai", Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if resp.Total != 0 || len(resp.Counts) != 0 {
		t.Errorf("empty batch must yield total 0 / no counts, got %+v", resp)
	}
}

func TestServer_RejectsOverlargeBatch(t *testing.T) {
	texts := make([]string, MaxBatchSize+1)
	_, err := NewServer(nil).Count(context.Background(), &CountRequest{Texts: texts, Model: "gpt-4o"})
	if err == nil {
		t.Fatal("expected an error for a batch over MaxBatchSize")
	}
}

func TestServer_Info(t *testing.T) {
	resp, err := NewServer(nil).Info(context.Background(), &InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if resp.ReadyAt == 0 {
		t.Error("ReadyAt must be set")
	}
}

func TestServer_ConcurrentSafe(t *testing.T) {
	srv := NewServer(tokencount.New())
	big := strings.Repeat("token ", 50)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if _, err := srv.Count(context.Background(), &CountRequest{
					Texts: []string{big, "short"}, Provider: "openai", Model: "gpt-4o",
				}); err != nil {
					t.Errorf("Count: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
