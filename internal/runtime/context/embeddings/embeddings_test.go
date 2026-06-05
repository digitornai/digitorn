package embeddings_test

import (
	"context"
	"math"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/context/embeddings"
	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

// =====================================================================
// Vector primitives
// =====================================================================

func TestCosine_Identical(t *testing.T) {
	v := embeddings.Vector{1, 2, 3, 4}
	if got := embeddings.Cosine(v, v); !approxEq(got, 1.0, 1e-3) {
		t.Errorf("cosine of identical vectors = %v, want ~1.0", got)
	}
}

func TestCosine_Orthogonal(t *testing.T) {
	a := embeddings.Vector{1, 0, 0}
	b := embeddings.Vector{0, 1, 0}
	if got := embeddings.Cosine(a, b); !approxEq(got, 0, 1e-3) {
		t.Errorf("cosine of orthogonal = %v, want 0", got)
	}
}

func TestCosine_Opposite(t *testing.T) {
	a := embeddings.Vector{1, 2, 3}
	b := embeddings.Vector{-1, -2, -3}
	if got := embeddings.Cosine(a, b); !approxEq(got, -1, 1e-3) {
		t.Errorf("cosine of opposite = %v, want -1", got)
	}
}

func TestCosine_LengthMismatch_Zero(t *testing.T) {
	a := embeddings.Vector{1, 2}
	b := embeddings.Vector{1, 2, 3}
	if got := embeddings.Cosine(a, b); got != 0 {
		t.Errorf("mismatch should return 0, got %v", got)
	}
}

func TestNormalize_UnitLength(t *testing.T) {
	v := embeddings.Vector{3, 4, 0}
	embeddings.Normalize(v)
	var sum float32
	for _, x := range v {
		sum += x * x
	}
	if !approxEq(sum, 1.0, 1e-3) {
		t.Errorf("normalized length² = %v, want 1.0 (vec=%v)", sum, v)
	}
}

func TestCosineNormalized_MatchesCosine_WhenInputsNormalized(t *testing.T) {
	a := embeddings.Vector{1, 2, 3, 4}
	b := embeddings.Vector{4, 3, 2, 1}
	embeddings.Normalize(a)
	embeddings.Normalize(b)
	fast := embeddings.CosineNormalized(a, b)
	slow := embeddings.Cosine(a, b)
	if !approxEq(fast, slow, 1e-4) {
		t.Errorf("fast=%v slow=%v (should match for normalized inputs)", fast, slow)
	}
}

// =====================================================================
// MockClient
// =====================================================================

func TestMock_DimensionAlways384(t *testing.T) {
	client := embeddings.MockClient{}
	vecs, err := client.Embed(context.Background(), []string{
		"short", "a longer sentence with more tokens", "x",
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i, v := range vecs {
		if len(v) != embeddings.EmbeddingDim {
			t.Errorf("vecs[%d] len = %d, want %d", i, len(v), embeddings.EmbeddingDim)
		}
	}
}

func TestMock_Deterministic(t *testing.T) {
	client := embeddings.MockClient{}
	v1, _ := client.Embed(context.Background(), []string{"hello world"})
	v2, _ := client.Embed(context.Background(), []string{"hello world"})
	for i := range v1[0] {
		if v1[0][i] != v2[0][i] {
			t.Fatalf("same input → different output at dim %d", i)
		}
	}
}

func TestMock_DifferentInputs_DifferentOutputs(t *testing.T) {
	client := embeddings.MockClient{}
	vs, _ := client.Embed(context.Background(), []string{"foo", "bar"})
	// Should be different (not perfect cosine 1.0).
	sim := embeddings.CosineNormalized(vs[0], vs[1])
	if sim > 0.99 {
		t.Errorf("unrelated inputs too similar : cos=%v", sim)
	}
}

func TestMock_SharedTokens_MoreSimilar(t *testing.T) {
	client := embeddings.MockClient{}
	vs, _ := client.Embed(context.Background(), []string{
		"read file",
		"read content",  // shares "read"
		"shell command", // shares nothing
	})
	simShared := embeddings.CosineNormalized(vs[0], vs[1])
	simUnrelated := embeddings.CosineNormalized(vs[0], vs[2])
	if simShared <= simUnrelated {
		t.Errorf("shared-token similarity %.3f should beat unrelated %.3f",
			simShared, simUnrelated)
	}
}

// =====================================================================
// SemanticIndex
// =====================================================================

func TestSemanticIndex_Build_AndSearch(t *testing.T) {
	client := embeddings.MockClient{}
	corpus := map[string]string{
		"filesystem.read":  "read file contents",
		"filesystem.write": "write file contents",
		"shell.bash":       "execute shell command",
	}
	si, err := embeddings.NewSemanticIndex(context.Background(), client, corpus)
	if err != nil {
		t.Fatalf("NewSemanticIndex: %v", err)
	}
	if si.Size() != 3 {
		t.Fatalf("Size = %d, want 3", si.Size())
	}

	// Query : "read file" should rank filesystem.read at the top.
	vecs, _ := client.Embed(context.Background(), []string{"read file"})
	hits := si.Search(vecs[0], 3)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].FQN != "filesystem.read" {
		t.Errorf("top = %s, want filesystem.read (cos=%v)",
			hits[0].FQN, hits[0].Score)
	}
}

func TestSemanticIndex_NilSafe(t *testing.T) {
	var si *embeddings.SemanticIndex
	if si.Size() != 0 {
		t.Error("nil Size should be 0")
	}
	hits := si.Search(embeddings.Vector{1, 2, 3}, 5)
	if hits != nil {
		t.Errorf("nil Search returned %v, want nil", hits)
	}
}

func TestSemanticIndex_EmptyCorpus(t *testing.T) {
	si, err := embeddings.NewSemanticIndex(context.Background(), embeddings.MockClient{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if si.Size() != 0 {
		t.Errorf("empty corpus size = %d, want 0", si.Size())
	}
}

// =====================================================================
// Hybrid scoring : index.Search with Semantic attached
// =====================================================================

func buildIndex(t *testing.T) *index.ToolIndex {
	t.Helper()
	universe := []policy.AvailableAction{
		{Module: "filesystem", Action: "read",
			Spec: &tool.Spec{
				Name:        "filesystem.read",
				Description: "Read the contents of a file from disk",
				RiskLevel:   tool.RiskLow,
				Tags:        []string{"io", "files"},
				Aliases:     []string{"lire"},
			}},
		{Module: "shell", Action: "bash",
			Spec: &tool.Spec{
				Name:        "shell.bash",
				Description: "Execute a Bash command in a shell",
				RiskLevel:   tool.RiskLow,
			}},
		{Module: "http", Action: "get",
			Spec: &tool.Spec{
				Name:        "http.get",
				Description: "Send an HTTP GET request to fetch a URL",
				RiskLevel:   tool.RiskLow,
			}},
	}
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	return index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, universe)
}

func TestAttach_AddsSemanticScores(t *testing.T) {
	idx := buildIndex(t)
	client := embeddings.MockClient{}
	si, err := embeddings.NewSemanticIndex(context.Background(), client,
		embeddings.BuildCorpus(idx.Tools))
	if err != nil {
		t.Fatalf("NewSemanticIndex: %v", err)
	}
	embeddings.Attach(idx, si, client)

	if idx.Semantic == nil {
		t.Fatal("Attach didn't set Semantic")
	}

	// Query for "fetch URL" — no keyword match for "fetch", but the
	// semantic side should bring http.get to the top thanks to the
	// shared "URL" token in its description.
	hits := idx.Search("fetch URL", 5)
	if len(hits) == 0 {
		t.Fatal("no hits despite Semantic wired")
	}
	if hits[0].Tool.FQN != "http.get" {
		t.Errorf("top hit = %s, want http.get (Semantic should bring it up)\nall: %+v",
			hits[0].Tool.FQN, hits)
	}
}

// TestAttach_KeywordExactStillWins : an exact FQN keyword hit (100
// pts) should still rank above a pure semantic 1.0 hit (10 pts).
// This is the documented "semantic dominates, but exact-name
// matches get a significant bonus".
func TestAttach_KeywordExactStillWins(t *testing.T) {
	idx := buildIndex(t)
	client := embeddings.MockClient{}
	si, _ := embeddings.NewSemanticIndex(context.Background(), client,
		embeddings.BuildCorpus(idx.Tools))
	embeddings.Attach(idx, si, client)

	hits := idx.Search("filesystem.read", 5)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Tool.FQN != "filesystem.read" {
		t.Errorf("exact FQN should win : top=%s", hits[0].Tool.FQN)
	}
}

// TestAttach_NoSemantic_KeywordOnlyPreserved : when Semantic is
// nil (no Attach call), Search returns the same results as CB-1.
func TestAttach_NoSemantic_KeywordOnlyPreserved(t *testing.T) {
	idx := buildIndex(t)
	// No Attach call.
	hits := idx.Search("read", 5)
	if len(hits) == 0 {
		t.Fatal("no hits in keyword-only mode")
	}
	if hits[0].Tool.FQN != "filesystem.read" {
		t.Errorf("keyword-only top = %s", hits[0].Tool.FQN)
	}
}

// TestBuildCorpus_IncludesAllFields : the corpus payload for one
// tool must include the FQN, description, tags, aliases, and
// param fields so the embedding model has the full signal.
func TestBuildCorpus_IncludesAllFields(t *testing.T) {
	idx := buildIndex(t)
	corpus := embeddings.BuildCorpus(idx.Tools)
	got := corpus["filesystem.read"]
	for _, want := range []string{
		"filesystem.read",                       // FQN
		"Read the contents of a file from disk", // Description
		"io", "files",                           // Tags
		"lire", // Alias
	} {
		if !contains(got, want) {
			t.Errorf("corpus missing %q : %q", want, got)
		}
	}
}

// =====================================================================
// helpers
// =====================================================================

func approxEq(a, b, eps float32) bool {
	return float32(math.Abs(float64(a-b))) < eps
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
