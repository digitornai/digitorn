package embeddings_test

import (
	"errors"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
)

// downSemantic simulates an embeddings worker that died after the index
// was built : the corpus vectors exist, but query-time embedding fails.
type downSemantic struct{}

func (downSemantic) SearchVector(_ []float32, _ int) []index.SemanticHit { return nil }
func (downSemantic) EmbedQuery(_ string) ([]float32, error) {
	return nil, errors.New("embeddings worker unavailable")
}

// TestSemanticSearch_WorkerDown_FallsBackToKeyword locks the graceful-
// degrade contract: when the semantic side errors at query time (worker
// crashed mid-session), Search must still return keyword hits and never
// panic. This is the doc-defined "semantic is best-effort" guarantee.
func TestSemanticSearch_WorkerDown_FallsBackToKeyword(t *testing.T) {
	idx := buildIndex(t)
	idx.Semantic = downSemantic{} // attached, but EmbedQuery fails

	hits := idx.Search("read", 5)
	if len(hits) == 0 {
		t.Fatal("expected keyword hits when the semantic side is down")
	}
	if hits[0].Tool.FQN != "filesystem.read" {
		t.Errorf("keyword fallback top = %s, want filesystem.read", hits[0].Tool.FQN)
	}
}

// TestSemanticSearch_WorkerDown_NoQueryStillKeyword : a second query
// shape (exact-ish keyword) also survives a downed worker.
func TestSemanticSearch_WorkerDown_NoQueryStillKeyword(t *testing.T) {
	idx := buildIndex(t)
	idx.Semantic = downSemantic{}

	hits := idx.Search("fetch URL", 5)
	if len(hits) == 0 {
		t.Fatal("expected keyword hits for 'fetch URL' with semantic down")
	}
	// http.get has "URL" in its description → keyword still finds it.
	found := false
	for _, h := range hits {
		if h.Tool.FQN == "http.get" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected http.get among keyword hits, got %v", hits)
	}
}
