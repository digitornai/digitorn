package meta_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/policy"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// Sanitize / Canonicalize / SplitFQN
// =====================================================================

func TestCanonicalize(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"filesystem.read", "filesystem.read"},
		{"filesystem__read", "filesystem.read"},
		{"context_builder.search_tools", "context_builder.search_tools"},
		{"context_builder__search_tools", "context_builder.search_tools"},
		{"search_tools", "search_tools"}, // bare meta-tool action
		{"a__b__c", "a.b__c"},            // only first __ converts
		{"", ""},
	} {
		if got := meta.Canonicalize(c.in); got != c.want {
			t.Errorf("Canonicalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitize(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"filesystem.read", "filesystem__read"},
		{"filesystem__read", "filesystem__read"}, // already
		{"bare_name", "bare_name"},
		{"", ""},
	} {
		if got := meta.Sanitize(c.in); got != c.want {
			t.Errorf("Sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRoundTrip_DotUnderscoreDot(t *testing.T) {
	for _, name := range []string{
		"filesystem.read", "shell.bash", "context_builder.search_tools",
	} {
		out := meta.Canonicalize(meta.Sanitize(name))
		if out != name {
			t.Errorf("round-trip lost %q (got %q)", name, out)
		}
	}
}

func TestSplitFQN(t *testing.T) {
	for _, c := range []struct {
		in, mod, act string
	}{
		{"filesystem.read", "filesystem", "read"},
		{"filesystem__read", "filesystem", "read"},
		{"bare", "", "bare"},
		{"", "", ""},
	} {
		m, a := meta.SplitFQN(c.in)
		if m != c.mod || a != c.act {
			t.Errorf("SplitFQN(%q) = (%q, %q), want (%q, %q)",
				c.in, m, a, c.mod, c.act)
		}
	}
}

func TestIsContextBuilderMeta(t *testing.T) {
	wantTrue := []string{
		"context_builder.search_tools",
		"context_builder.get_tool",
		"context_builder.execute_tool",
		"context_builder.list_categories",
		"context_builder.browse_category",
		// P-1 primitives are also intercepted by the meta dispatcher
		// since they're the doc-defined always-direct tools.
		"context_builder.run_parallel",
		"context_builder.ask_user",
		"context_builder.background_run",
		"context_builder.use_skill",
		"context_builder.call_app",
	}
	wantFalse := []string{
		"filesystem.read",
		"context_builder",
		"",
		"random_tool",
		"context_builder.unknown_primitive",
	}
	for _, n := range wantTrue {
		if !meta.IsContextBuilderMeta(n) {
			t.Errorf("IsContextBuilderMeta(%q) = false, want true", n)
		}
	}
	for _, n := range wantFalse {
		if meta.IsContextBuilderMeta(n) {
			t.Errorf("IsContextBuilderMeta(%q) = true, want false", n)
		}
	}
}

// =====================================================================
// MetaDispatcher : meta-tool handlers
// =====================================================================

// fakeInner is a counting dispatcher for the auto-routing tests.
type fakeInner struct {
	mu    sync.Mutex
	count int
	calls []runtime.ToolInvocation
}

func (f *fakeInner) Dispatch(_ context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	f.mu.Lock()
	f.count++
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	return runtime.ToolOutcome{
		Status: "completed",
		Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeText, Text: "inner ok"},
		},
	}
}

// lookupOf wraps a fixed ToolIndex into the IndexLookup signature
// the MetaDispatcher expects. Tests don't care about per-(app,
// agent) routing ; they always want the same index back.
func lookupOf(idx *index.ToolIndex) func(string, string) *index.ToolIndex {
	return func(string, string) *index.ToolIndex { return idx }
}

// buildTestIndex returns a small ToolIndex with deterministic
// content for the meta-tool tests.
func buildTestIndex(t *testing.T) *index.ToolIndex {
	t.Helper()
	universe := []policy.AvailableAction{
		{Module: "filesystem", Action: "read",
			Spec: &tool.Spec{Name: "filesystem.read",
				Description: "Read the contents of a file.",
				RiskLevel:   tool.RiskLow,
				Tags:        []string{"io", "files"},
				Aliases:     []string{"lire"},
				Params: []tool.ParamSpec{
					{Name: "path", Type: "string", Description: "File path", Required: true},
				},
			}},
		{Module: "filesystem", Action: "write",
			Spec: &tool.Spec{Name: "filesystem.write",
				Description: "Write content to a file.",
				RiskLevel:   tool.RiskMedium,
				Tags:        []string{"io", "files"},
			}},
		{Module: "shell", Action: "bash",
			Spec: &tool.Spec{Name: "shell.bash",
				Description: "Execute a Bash command.",
				RiskLevel:   tool.RiskLow, // low so gate 2 passes
			}},
	}
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	return index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, universe)
}

// decodeOutcome extracts the JSON object from a meta-tool's text Part.
func decodeOutcome(t *testing.T, o runtime.ToolOutcome) map[string]any {
	t.Helper()
	if o.Status != "completed" {
		t.Fatalf("status = %q, want completed (err=%q)", o.Status, o.Error)
	}
	if len(o.Parts) == 0 || o.Parts[0].Type != sessionstore.PartTypeText {
		t.Fatalf("no text part in outcome")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(o.Parts[0].Text), &obj); err != nil {
		t.Fatalf("decode: %v\nraw: %s", err, o.Parts[0].Text)
	}
	return obj
}

// --- search_tools --------------------------------------------------

func TestSearchTools_BasicHit(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.search_tools",
		Args: map[string]any{"query": "read file"},
	})
	body := decodeOutcome(t, out)
	hits, _ := body["hits"].([]any)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	first := hits[0].(map[string]any)
	if first["name"] != "filesystem.read" {
		t.Errorf("top hit name = %v, want filesystem.read", first["name"])
	}
}

// TestSearchTools_NoQuery_ListsCategories : search_tools is now the unified
// discovery tool — with NO query (and no category) it lists the available
// domains/categories instead of erroring.
func TestSearchTools_NoQuery_ListsCategories(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.search_tools",
		Args: map[string]any{},
	})
	if out.Status != "completed" {
		t.Fatalf("no query → list categories : status = %q, want completed", out.Status)
	}
	if len(out.Parts) == 0 || !strings.Contains(out.Parts[0].Text, "categories") {
		t.Errorf("no-query search_tools should return categories, got %+v", out.Parts)
	}
}

// TestSearchTools_Detail_InlinesParams : detail=true ships the full callable
// signature in each hit, so the model can invoke the tool WITHOUT a get_tool
// round-trip (the "ultra-powerful, one-hop discovery" upgrade).
func TestSearchTools_Detail_InlinesParams(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.search_tools",
		Args: map[string]any{"query": "files", "detail": true},
	})
	if out.Status != "completed" {
		t.Fatalf("detail search : status = %q", out.Status)
	}
	txt := out.Parts[0].Text
	if !strings.Contains(txt, "\"params\"") {
		t.Errorf("detail=true must inline params, got %s", txt)
	}
}

// TestSearchTools_CategoryScopedQuery : query + category restricts results to
// that domain.
func TestSearchTools_CategoryScopedQuery(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.search_tools",
		Args: map[string]any{"query": "read", "category": "filesystem"},
	})
	if out.Status != "completed" {
		t.Fatalf("scoped search : status = %q", out.Status)
	}
	var got struct {
		Hits []struct {
			Name string `json:"name"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(out.Parts[0].Text), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, h := range got.Hits {
		if !strings.HasPrefix(h.Name, "filesystem.") {
			t.Errorf("category-scoped search leaked %q (not in filesystem)", h.Name)
		}
	}
}

// TestSearchTools_Category_Browses : search_tools with a category lists that
// domain's tools (former browse_category behaviour).
func TestSearchTools_Category_Browses(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.search_tools",
		Args: map[string]any{"category": "filesystem"},
	})
	if out.Status != "completed" {
		t.Fatalf("category browse : status = %q, want completed", out.Status)
	}
	if len(out.Parts) == 0 || !strings.Contains(out.Parts[0].Text, "filesystem") {
		t.Errorf("category search_tools should list that domain's tools, got %+v", out.Parts)
	}
}

func TestSearchTools_LimitRespected(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.search_tools",
		Args: map[string]any{"query": "files", "limit": float64(1)},
	})
	body := decodeOutcome(t, out)
	hits, _ := body["hits"].([]any)
	if len(hits) != 1 {
		t.Errorf("len(hits) = %d, want 1 (limit)", len(hits))
	}
}

// --- get_tool ------------------------------------------------------

func TestGetTool_ReturnsFullSchema(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.get_tool",
		Args: map[string]any{"name": "filesystem.read"},
	})
	body := decodeOutcome(t, out)
	if body["name"] != "filesystem.read" {
		t.Errorf("name = %v", body["name"])
	}
	params, _ := body["params"].([]any)
	if len(params) != 1 {
		t.Fatalf("params count = %d, want 1", len(params))
	}
	p := params[0].(map[string]any)
	if p["name"] != "path" {
		t.Errorf("param name = %v", p["name"])
	}
	if !p["required"].(bool) {
		t.Errorf("path should be required")
	}
}

func TestGetTool_UnderscoreNameAccepted(t *testing.T) {
	// The LLM might emit the OpenAI-sanitized form ; we must accept it.
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.get_tool",
		Args: map[string]any{"name": "filesystem__read"},
	})
	body := decodeOutcome(t, out)
	if body["name"] != "filesystem.read" {
		t.Errorf("name = %v (should canonicalize)", body["name"])
	}
}

func TestGetTool_Unknown_Errored(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.get_tool",
		Args: map[string]any{"name": "nonexistent.tool"},
	})
	if out.Status != "errored" {
		t.Errorf("unknown : status = %q", out.Status)
	}
	if !strings.Contains(out.Error, "not found") {
		t.Errorf("error should mention 'not found' : %q", out.Error)
	}
}

// --- list_categories -----------------------------------------------

func TestListCategories_ReturnsAllModules(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.list_categories",
	})
	body := decodeOutcome(t, out)
	cats, _ := body["categories"].([]any)
	if len(cats) != 2 {
		t.Fatalf("len(categories) = %d, want 2 (filesystem + shell)", len(cats))
	}
}

// --- browse_category -----------------------------------------------

func TestBrowseCategory_FirstPage(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t)), BrowsePageSize: 10}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.browse_category",
		Args: map[string]any{"category": "filesystem"},
	})
	body := decodeOutcome(t, out)
	tools, _ := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2 (read + write)", len(tools))
	}
	if body["total"].(float64) != 2 {
		t.Errorf("total = %v, want 2", body["total"])
	}
}

func TestBrowseCategory_Unknown_Errored(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.browse_category",
		Args: map[string]any{"category": "unknown"},
	})
	if out.Status != "errored" {
		t.Errorf("unknown category : status = %q", out.Status)
	}
}

func TestBrowseCategory_Pagination(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t)), BrowsePageSize: 1}
	// Page 1 : first tool.
	out1 := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.browse_category",
		Args: map[string]any{"category": "filesystem", "page": float64(1)},
	})
	body1 := decodeOutcome(t, out1)
	tools1, _ := body1["tools"].([]any)
	if len(tools1) != 1 {
		t.Fatalf("page 1 : len = %d, want 1", len(tools1))
	}
	// Page 2 : second tool.
	out2 := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.browse_category",
		Args: map[string]any{"category": "filesystem", "page": float64(2)},
	})
	body2 := decodeOutcome(t, out2)
	tools2, _ := body2["tools"].([]any)
	if len(tools2) != 1 {
		t.Fatalf("page 2 : len = %d, want 1", len(tools2))
	}
	if tools1[0].(map[string]any)["name"] == tools2[0].(map[string]any)["name"] {
		t.Error("page 1 and page 2 returned the same tool")
	}
	// Page 3 : past end, empty.
	out3 := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.browse_category",
		Args: map[string]any{"category": "filesystem", "page": float64(3)},
	})
	body3 := decodeOutcome(t, out3)
	tools3, _ := body3["tools"].([]any)
	if len(tools3) != 0 {
		t.Errorf("page 3 : len = %d, want 0 (past end)", len(tools3))
	}
}

// --- execute_tool --------------------------------------------------

// TestExecuteTool_DispatchesViaInner : execute_tool's job is to
// re-enter Dispatch with the resolved target name. The inner
// dispatcher must see the call.
func TestExecuteTool_DispatchesViaInner(t *testing.T) {
	inner := &fakeInner{}
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t)), Inner: inner}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.execute_tool",
		Args: map[string]any{
			"name":   "filesystem.read",
			"params": map[string]any{"path": "/etc/hosts"},
		},
	})
	if out.Status != "completed" {
		t.Fatalf("status = %q, err=%q", out.Status, out.Error)
	}
	if inner.count != 1 {
		t.Fatalf("inner.count = %d, want 1", inner.count)
	}
	if inner.calls[0].Name != "filesystem.read" {
		t.Errorf("inner saw name = %q, want filesystem.read", inner.calls[0].Name)
	}
	if inner.calls[0].Args["path"] != "/etc/hosts" {
		t.Errorf("path lost : %v", inner.calls[0].Args)
	}
}

func TestExecuteTool_MissingName_Errored(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.execute_tool",
		Args: map[string]any{"params": map[string]any{}},
	})
	if out.Status != "errored" {
		t.Errorf("missing name : status = %q", out.Status)
	}
}

// TestExecuteTool_FlattenedArgs : real LLMs frequently skip the
// `params` wrapper and put the inner args at the top level. The
// dispatcher must accept this shape too.
func TestExecuteTool_FlattenedArgs(t *testing.T) {
	inner := &fakeInner{}
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t)), Inner: inner}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.execute_tool",
		Args: map[string]any{
			"name": "filesystem.read",
			"path": "/etc/hosts", // flat — no `params` wrapper
		},
	})
	if out.Status != "completed" {
		t.Fatalf("status = %q, err=%q", out.Status, out.Error)
	}
	if inner.count != 1 || inner.calls[0].Args["path"] != "/etc/hosts" {
		t.Errorf("flat args lost : %+v", inner.calls[0].Args)
	}
	// "name" must NOT leak through to the inner — it's the meta key.
	if _, ok := inner.calls[0].Args["name"]; ok {
		t.Errorf("meta `name` key leaked into inner args")
	}
}

// TestExecuteTool_ArgumentsAlias : "arguments" is accepted as a
// synonym of "params" (OpenAI's own tool_call surface uses
// "arguments", and some LLMs carry that habit over to nested
// execute_tool calls).
func TestExecuteTool_ArgumentsAlias(t *testing.T) {
	inner := &fakeInner{}
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t)), Inner: inner}
	d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.execute_tool",
		Args: map[string]any{
			"name":      "filesystem.read",
			"arguments": map[string]any{"path": "/x"},
		},
	})
	if inner.count != 1 || inner.calls[0].Args["path"] != "/x" {
		t.Errorf("arguments-alias lost : %+v", inner.calls)
	}
}

// TestExecuteTool_ParamsTakesPrecedence : when both `params` and
// flattened keys are present, `params` wins (the canonical shape
// is authoritative).
func TestExecuteTool_ParamsTakesPrecedence(t *testing.T) {
	inner := &fakeInner{}
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t)), Inner: inner}
	d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "context_builder.execute_tool",
		Args: map[string]any{
			"name":   "filesystem.read",
			"params": map[string]any{"path": "/canonical"},
			"path":   "/flat", // should be ignored
		},
	})
	if inner.calls[0].Args["path"] != "/canonical" {
		t.Errorf("params precedence broken : got %v", inner.calls[0].Args["path"])
	}
}

// --- auto-routing --------------------------------------------------

// TestAutoRoute_DotForm_GoesToInner : LLM calls "filesystem.read"
// directly → dispatcher forwards to Inner without going through
// execute_tool first.
func TestAutoRoute_DotForm_GoesToInner(t *testing.T) {
	inner := &fakeInner{}
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t)), Inner: inner}
	d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "filesystem.read",
		Args: map[string]any{"path": "/x"},
	})
	if inner.count != 1 {
		t.Fatalf("inner not called : count=%d", inner.count)
	}
	if inner.calls[0].Name != "filesystem.read" {
		t.Errorf("inner saw %q, want filesystem.read", inner.calls[0].Name)
	}
}

// TestAutoRoute_UnderscoreForm_Canonicalized : "filesystem__read"
// → Inner sees "filesystem.read".
func TestAutoRoute_UnderscoreForm_Canonicalized(t *testing.T) {
	inner := &fakeInner{}
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t)), Inner: inner}
	d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "filesystem__read",
		Args: map[string]any{"path": "/x"},
	})
	if inner.count != 1 {
		t.Fatal("inner not called")
	}
	if inner.calls[0].Name != "filesystem.read" {
		t.Errorf("inner saw %q, want filesystem.read (canonicalized)",
			inner.calls[0].Name)
	}
}

// TestNoInner_ReturnsClearError : domain tool with no inner
// dispatcher = the doc-default error ("tool dispatcher not wired").
func TestNoInner_ReturnsClearError(t *testing.T) {
	d := &meta.MetaDispatcher{IndexLookup: lookupOf(buildTestIndex(t))} // Inner nil
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "filesystem.read",
		Args: map[string]any{"path": "/x"},
	})
	if out.Status != "errored" {
		t.Fatalf("nil Inner : status = %q, want errored", out.Status)
	}
	if !strings.Contains(out.Error, "not wired") {
		t.Errorf("error should say 'not wired' : %q", out.Error)
	}
}
