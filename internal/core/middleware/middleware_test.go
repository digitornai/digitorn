package middleware

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/ports"
)

func mctxWithUser(text string) *ports.MiddlewareContext {
	return &ports.MiddlewareContext{
		SystemPrompt: "base",
		Messages:     []ports.LLMMessage{{Role: "user", Content: text}},
	}
}

func TestMaskSecrets_BeforeAndAfter(t *testing.T) {
	mw, _ := newMaskSecrets(map[string]any{"patterns": []any{"customsecret"}, "replacement": "X"}, Deps{})
	mctx := mctxWithUser("my password=hunter2 and customsecret=foo")
	if _, sc, _ := mw.Before(context.Background(), mctx); sc {
		t.Fatal("mask_secrets must not short-circuit")
	}
	got := mctx.Messages[0].Content
	if strings.Contains(got, "hunter2") || strings.Contains(got, "foo") {
		t.Errorf("secrets not masked in user message: %q", got)
	}
	if !strings.Contains(got, "X") {
		t.Errorf("replacement not applied: %q", got)
	}
	out, _ := mw.After(context.Background(), mctx, "here is your token=abc123xyz", nil)
	if strings.Contains(out, "abc123xyz") {
		t.Errorf("secret not masked in response: %q", out)
	}
}

func TestPromptInject_AppendPrepend(t *testing.T) {
	app, _ := newPromptInject(map[string]any{"system": "EXTRA"}, Deps{})
	mctx := &ports.MiddlewareContext{SystemPrompt: "base"}
	app.Before(context.Background(), mctx)
	if mctx.SystemPrompt != "base\n\nEXTRA" {
		t.Errorf("append: %q", mctx.SystemPrompt)
	}
	pre, _ := newPromptInject(map[string]any{"system": "EXTRA", "position": "prepend"}, Deps{})
	mctx2 := &ports.MiddlewareContext{SystemPrompt: "base"}
	pre.Before(context.Background(), mctx2)
	if mctx2.SystemPrompt != "EXTRA\n\nbase" {
		t.Errorf("prepend: %q", mctx2.SystemPrompt)
	}
}

func TestContentFilter_ShortCircuit(t *testing.T) {
	mw, _ := newContentFilter(map[string]any{"block_patterns": []any{"drop table"}, "rejection_message": "NO"}, Deps{})
	resp, sc, _ := mw.Before(context.Background(), mctxWithUser("please DROP TABLE users"))
	if !sc || resp != "NO" {
		t.Fatalf("must short-circuit with rejection, got (%q, %v)", resp, sc)
	}
	if _, sc, _ := mw.Before(context.Background(), mctxWithUser("hello world")); sc {
		t.Error("clean message must not be blocked")
	}
}

func TestResponseFilter_CapAndMask(t *testing.T) {
	mw, _ := newResponseFilter(map[string]any{"max_length": 10, "mask_secrets": true}, Deps{})
	out, _ := mw.After(context.Background(), nil, "this is a very long response that exceeds", nil)
	if !strings.Contains(out, "[Response truncated]") {
		t.Errorf("must truncate: %q", out)
	}
	out2, _ := mw.After(context.Background(), nil, "token=secretvalue12345", nil)
	if strings.Contains(out2, "secretvalue12345") {
		t.Errorf("must mask secrets in response: %q", out2)
	}
}

func TestRagInject_RetrieverSeam(t *testing.T) {
	// nil retriever => inert.
	inert, _ := newRagInject(nil, Deps{})
	mctx := mctxWithUser("what is X?")
	inert.Before(context.Background(), mctx)
	if mctx.SystemPrompt != "base" {
		t.Errorf("nil retriever must be inert, got %q", mctx.SystemPrompt)
	}
	// with a retriever => injects.
	r := Retriever(func(_ context.Context, q string) ([]string, error) {
		return []string{"chunk about " + q}, nil
	})
	mw, _ := newRagInject(map[string]any{"max_chunks": 3}, Deps{Retriever: r})
	mctx2 := mctxWithUser("what is X?")
	mw.Before(context.Background(), mctx2)
	if !strings.Contains(mctx2.SystemPrompt, "Relevant context") || !strings.Contains(mctx2.SystemPrompt, "chunk about what is X?") {
		t.Errorf("rag must inject retrieved context: %q", mctx2.SystemPrompt)
	}
}

// twoMarker appends its name in After to prove reverse ordering.
type marker struct{ id string }

func (m marker) Name() string { return "m" + m.id }
func (m marker) Before(context.Context, *ports.MiddlewareContext) (string, bool, error) {
	return "", false, nil
}
func (m marker) After(_ context.Context, _ *ports.MiddlewareContext, resp string, _ []ports.LLMToolCall) (string, error) {
	return resp + m.id, nil
}

func TestPipeline_AfterReverseOrder(t *testing.T) {
	p := New([]ports.AppMiddleware{marker{"A"}, marker{"B"}}, nil)
	out, _ := p.After(context.Background(), &ports.MiddlewareContext{}, "base", nil)
	// reverse: B applied first, then A -> "baseBA"
	if out != "baseBA" {
		t.Errorf("After must run in reverse order, got %q want baseBA", out)
	}
}

func TestPipeline_BeforeShortCircuitStops(t *testing.T) {
	blocker, _ := newContentFilter(map[string]any{"block_patterns": []any{"x"}}, Deps{})
	mask, _ := newMaskSecrets(nil, Deps{})
	p := New([]ports.AppMiddleware{blocker, mask}, nil)
	_, sc, _ := p.Before(context.Background(), mctxWithUser("contains x here"))
	if !sc {
		t.Error("a blocking middleware must short-circuit the chain")
	}
}

func TestBuild_SkipsDisabledAndUnknown(t *testing.T) {
	no := false
	p := Build([]schema.MiddlewareEntry{
		{Name: "mask_secrets"},
		{Name: "totally_unknown"},
		{Name: "prompt_inject", Enabled: &no},
	}, Deps{}, nil)
	if p == nil {
		t.Fatal("pipeline must be built from the one valid entry")
	}
	names := p.Names()
	if len(names) != 1 || names[0] != "mask_secrets" {
		t.Errorf("expected only mask_secrets, got %v", names)
	}
}

func TestMiddlewareEntry_YAMLForms(t *testing.T) {
	const doc = `
- mask_secrets
- content_filter: { block_patterns: ["rm -rf"] }
- response_filter: { max_length: 100, enabled: false }
- name: prompt_inject
  config: { system: "hi" }
`
	var entries []schema.MiddlewareEntry
	if err := yaml.Unmarshal([]byte(doc), &entries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("want 4 entries, got %d", len(entries))
	}
	if entries[0].Name != "mask_secrets" {
		t.Errorf("bare string form: %+v", entries[0])
	}
	if entries[1].Name != "content_filter" || entries[1].Config["block_patterns"] == nil {
		t.Errorf("name-as-key form: %+v", entries[1])
	}
	if entries[2].Name != "response_filter" || entries[2].Enabled == nil || *entries[2].Enabled {
		t.Errorf("enabled lifted from name-as-key config: %+v", entries[2])
	}
	if entries[3].Name != "prompt_inject" || entries[3].Config["system"] != "hi" {
		t.Errorf("structured form: %+v", entries[3])
	}
}
