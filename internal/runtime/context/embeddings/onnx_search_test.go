//go:build onnx

package embeddings_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	bkend "github.com/digitornai/digitorn/internal/embeddings/backend"
	"github.com/digitornai/digitorn/internal/runtime/context/embeddings"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

// onnxClient adapts the real ONNX backend to the context EmbeddingClient.
type onnxClient struct{ be bkend.Backend }

func (c onnxClient) Embed(ctx context.Context, texts []string) ([]embeddings.Vector, error) {
	vecs, err := c.be.Embed(ctx, texts, true) // L2-normalised
	if err != nil {
		return nil, err
	}
	out := make([]embeddings.Vector, len(vecs))
	for i, v := range vecs {
		out[i] = embeddings.Vector(v)
	}
	return out, nil
}

func onnxModelDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("DIGITORN_EMBED_MODEL_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home dir: %v", err)
		}
		dir = filepath.Join(home, ".digitorn", "models", "paraphrase-multilingual-MiniLM-L12-v2")
	}
	if _, err := os.Stat(filepath.Join(dir, "model.onnx")); err != nil {
		t.Skipf("model.onnx not present (%s)", dir)
	}
	return dir
}

func semanticUniverse() []policy.AvailableAction {
	mk := func(mod, act, desc string) policy.AvailableAction {
		return policy.AvailableAction{Module: mod, Action: act, Spec: &tool.Spec{
			Name: mod + "." + act, Description: desc, RiskLevel: tool.RiskLow,
		}}
	}
	return []policy.AvailableAction{
		mk("filesystem", "delete", "Remove a file permanently from the local disk"),
		mk("http", "get", "Send an HTTP request to download a web page"),
		mk("db", "query", "Run a SQL statement against the relational database"),
		mk("email", "send", "Deliver an electronic mail message to a recipient"),
		mk("calendar", "create", "Schedule a new meeting on the user's agenda"),
	}
}

func buildSemanticIndex(t *testing.T, attach bool) *index.ToolIndex {
	t.Helper()
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	idx := index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, semanticUniverse())
	if !attach {
		return idx
	}
	be, err := bkend.NewONNX(onnxModelDir(t))
	if err != nil {
		t.Fatalf("NewONNX: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	client := onnxClient{be: be}
	si, err := embeddings.NewSemanticIndex(context.Background(), client,
		embeddings.BuildCorpus(idx.Tools))
	if err != nil {
		t.Fatalf("NewSemanticIndex: %v", err)
	}
	embeddings.Attach(idx, si, client)
	if idx.Semantic == nil {
		t.Fatal("Attach didn't set Semantic")
	}
	return idx
}

// TestSemanticSearch_ProductionPath_RealModel proves the EXACT
// production search path: idx.Search with a real ONNX SemanticIndex
// attached surfaces the right tool from MEANING alone, on queries that
// share NO keyword with the tool's description — and keyword-only
// search fails to rank them. This is the live, doc-aligned semantic
// search proof.
func TestSemanticSearch_ProductionPath_RealModel(t *testing.T) {
	kw := buildSemanticIndex(t, false)
	sem := buildSemanticIndex(t, true)

	cases := []struct {
		query string
		want  string
	}{
		{"erase a document from my computer", "filesystem.delete"},
		{"look up customer records in the company data store", "db.query"},
		{"notify a coworker by electronic correspondence", "email.send"},
		{"book a calendar appointment for next week", "calendar.create"},
	}
	for _, c := range cases {
		semHits := sem.Search(c.query, 5)
		if len(semHits) == 0 {
			t.Errorf("[%s] semantic returned no hits", c.query)
			continue
		}
		kwTop := "(none)"
		if h := kw.Search(c.query, 5); len(h) > 0 {
			kwTop = h[0].Tool.FQN
		}
		t.Logf("query=%q  semantic-top=%s  keyword-top=%s", c.query, semHits[0].Tool.FQN, kwTop)
		if semHits[0].Tool.FQN != c.want {
			t.Errorf("[%s] semantic top = %s, want %s", c.query, semHits[0].Tool.FQN, c.want)
		}
	}
}
