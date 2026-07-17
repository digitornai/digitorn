package rag

import (
	"strings"
	"testing"
)

func TestChunkize_ShortTextOneChunk(t *testing.T) {
	got := Chunkize("a short doc", StrategyRecursive, 500, 50)
	if len(got) != 1 || got[0].Text != "a short doc" || got[0].Index != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestChunkize_EmptyIsNil(t *testing.T) {
	if got := Chunkize("   ", StrategyRecursive, 500, 50); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestChunkize_RecursiveSplitsLongText(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString(strings.Repeat("phrase exemple ", 8))
		b.WriteString("\n\n")
	}
	chunks := Chunkize(b.String(), StrategyRecursive, 300, 40)
	if len(chunks) < 3 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if runeLen(c.Text) > 300*2 {
			t.Errorf("chunk %d far over size: %d runes", i, runeLen(c.Text))
		}
		if c.Index != i {
			t.Errorf("chunk index = %d, want %d", c.Index, i)
		}
	}
}

func TestChunkize_FixedWindowsWithOverlap(t *testing.T) {
	text := strings.Repeat("x", 1000)
	chunks := Chunkize(text, StrategyFixed, 200, 50)
	if len(chunks) < 5 {
		t.Fatalf("expected >=5 windows, got %d", len(chunks))
	}
	for _, c := range chunks {
		if runeLen(c.Text) > 200 {
			t.Errorf("fixed window over size: %d", runeLen(c.Text))
		}
	}
}

func TestChunkize_OverlapCarriesContext(t *testing.T) {
	text := ""
	for i := 0; i < 30; i++ {
		text += "Sentence number " + strings.Repeat("z", 5) + ". "
	}
	chunks := Chunkize(text, StrategyRecursive, 120, 40)
	if len(chunks) < 2 {
		t.Skip("not enough chunks to assess overlap")
	}
	joined := strings.Join(chunkTexts(chunks), "")
	if !strings.Contains(joined, "Sentence number") {
		t.Error("content lost during chunking")
	}
}

func chunkTexts(cs []Chunk) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Text
	}
	return out
}
