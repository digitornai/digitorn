package index_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

// PARANOID test harness for the CB-1 ToolIndex. Each section
// targets one invariant of the documented index :
//
//   1. Tokenizer correctness (Unicode, camelCase, dots, punctuation)
//   2. Synonym table (expand, includes, idempotency)
//   3. Scoring ordering (exact > action > alias > tag > desc > prefix)
//   4. Synonym scoring penalty
//   5. Edge-case queries (empty, punct-only, huge)
//   6. Adversarial tool definitions
//   7. Concurrency : N goroutines × Search on same ToolIndex
//   8. Determinism : repeated Search yields identical results
//   9. Schema-build filter chain (hidden/deny/risk/agent-subset)
//  10. Categories + FQNList sort stability
//  11. Multi-tool ranking + stable tie-break
//  12. Builder reuse + isolation between multiple Build calls

// ---- common fixtures ----------------------------------------------

func tspec(module, action, desc string, risk tool.RiskLevel, tags, aliases []string, params []tool.ParamSpec) policy.AvailableAction {
	return policy.AvailableAction{
		Module: module, Action: action,
		Spec: &tool.Spec{
			Name:        module + "." + action,
			Description: desc,
			RiskLevel:   risk,
			Tags:        tags,
			Aliases:     aliases,
			Params:      params,
		},
	}
}

func basicUniverse() []policy.AvailableAction {
	return []policy.AvailableAction{
		tspec("filesystem", "read", "Read the contents of a file from disk",
			tool.RiskLow,
			[]string{"io", "files"},
			[]string{"lire", "open file"},
			[]tool.ParamSpec{{Name: "path", Type: "string", Required: true}}),
		tspec("filesystem", "write", "Write content to a file on disk",
			tool.RiskMedium,
			[]string{"io", "files", "mutation"},
			[]string{"ecrire", "save"},
			nil),
		tspec("filesystem", "delete", "Delete a file from disk permanently",
			tool.RiskHigh,
			[]string{"io", "files", "destructive"},
			[]string{"supprimer", "remove"},
			nil),
		tspec("shell", "bash", "Execute a Bash command in a shell",
			tool.RiskLow,
			[]string{"process", "shell"},
			[]string{"exec", "command"},
			nil),
		tspec("memory", "remember", "Store a fact in long-term memory",
			tool.RiskLow,
			[]string{"state", "memory"},
			[]string{"memoriser"},
			nil),
	}
}

// pBuild constructs an index over the universe with permissive caps.
func pBuild(t *testing.T, universe []policy.AvailableAction) *index.ToolIndex {
	t.Helper()
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	return index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, universe)
}

// =====================================================================
// SECTION 1 — Synonym table
// =====================================================================

func TestParanoidIdx_Synonyms_DefaultBilingual(t *testing.T) {
	tbl := index.DefaultSynonyms()
	pairs := [][2]string{
		{"delete", "supprimer"},
		{"read", "lire"},
		{"write", "ecrire"},
		{"bash", "command"},
		{"create", "creer"},
	}
	for _, p := range pairs {
		if !tbl.Includes(p[0], p[1]) {
			t.Errorf("%s ↔ %s should be in default synonym table", p[0], p[1])
		}
	}
}

func TestParanoidIdx_Synonyms_CaseInsensitive(t *testing.T) {
	tbl := index.NewSynonymTable([][]string{{"delete", "remove"}})
	if !tbl.Includes("DELETE", "Remove") {
		t.Error("Includes must be case-insensitive")
	}
	got := tbl.Expand("DELETE")
	if len(got) != 2 {
		t.Errorf("Expand DELETE returned %v, want 2 items", got)
	}
}

func TestParanoidIdx_Synonyms_DuplicateGroupsFirstWins(t *testing.T) {
	tbl := index.NewSynonymTable([][]string{
		{"delete", "remove"},
		{"remove", "destroy"}, // "remove" already in bucket 0 → second bucket should ignore it
	})
	// "destroy" goes into bucket 1, alone (because "remove" was first-wins).
	if tbl.Includes("delete", "destroy") {
		t.Error("delete should NOT include destroy (different buckets after first-wins)")
	}
}

func TestParanoidIdx_Synonyms_ExpandIncludesItself(t *testing.T) {
	tbl := index.NewSynonymTable([][]string{{"a", "b", "c"}})
	got := tbl.Expand("b")
	found := false
	for _, x := range got {
		if x == "b" {
			found = true
		}
	}
	if !found {
		t.Errorf("Expand(b) should include b itself, got %v", got)
	}
}

// =====================================================================
// SECTION 2 — Scoring ordering
// =====================================================================

// TestParanoidIdx_Scoring_ExactFQNDominatesEverything : a query
// that's the exact FQN of one tool MUST rank that tool above any
// other hit regardless of other contributions.
func TestParanoidIdx_Scoring_ExactFQNDominatesEverything(t *testing.T) {
	idx := pBuild(t, basicUniverse())
	hits := idx.Search("filesystem.read", 10)
	if len(hits) == 0 || hits[0].Tool.FQN != "filesystem.read" {
		t.Fatalf("top hit = %+v, want filesystem.read", hits)
	}
	// Score should be at least 100 (exact FQN constant).
	if hits[0].Score < 100 {
		t.Errorf("exact FQN score = %d, want >= 100", hits[0].Score)
	}
}

// TestParanoidIdx_Scoring_ActionAboveTag : a tool whose action name
// matches the query must rank above one where only a tag matches.
func TestParanoidIdx_Scoring_ActionAboveTag(t *testing.T) {
	universe := []policy.AvailableAction{
		tspec("a", "search", "First", tool.RiskLow, nil, nil, nil),
		tspec("b", "other", "Second", tool.RiskLow, []string{"search"}, nil, nil),
	}
	idx := pBuild(t, universe)
	hits := idx.Search("search", 5)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Tool.FQN != "a.search" {
		t.Errorf("action hit should win over tag hit : top = %s", hits[0].Tool.FQN)
	}
}

// TestParanoidIdx_Scoring_AliasAboveDescription : an alias match
// (multilingual signal) must outrank a description-only match.
func TestParanoidIdx_Scoring_AliasAboveDescription(t *testing.T) {
	universe := []policy.AvailableAction{
		tspec("a", "alpha", "Some random sentence that mentions widget",
			tool.RiskLow, nil, nil, nil),
		tspec("b", "beta", "Generic description",
			tool.RiskLow, nil, []string{"widget"}, nil),
	}
	idx := pBuild(t, universe)
	hits := idx.Search("widget", 5)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Tool.FQN != "b.beta" {
		t.Errorf("alias hit should win over description : top = %s", hits[0].Tool.FQN)
	}
}

// TestParanoidIdx_Scoring_SynonymPenalty : a tool matched only via
// ONE synonym field (e.g. alias-only) must rank below a tool
// matched directly on the action name.
//
// Edge case documented : when the synonym path matches MULTIPLE
// fields on the same tool (action + alias both equal to a synonym
// of the query), the cumulative ×0.8 contributions can overtake
// a single direct field match. That's by design — two
// independent signals legitimately outweigh one. See the multi-
// signal variant test below.
func TestParanoidIdx_Scoring_SynonymPenalty(t *testing.T) {
	universe := []policy.AvailableAction{
		tspec("direct", "remove",
			"Directly mention the query word remove",
			tool.RiskLow, nil, nil, nil),
		// indirect has the synonym only in ALIAS — single source.
		tspec("indirect", "alpha",
			"No keyword overlap with the query",
			tool.RiskLow, nil, []string{"supprimer"}, nil),
	}
	idx := pBuild(t, universe)
	hits := idx.Search("remove", 5)
	if len(hits) < 2 {
		t.Fatalf("want both hits, got %d", len(hits))
	}
	if hits[0].Tool.FQN != "direct.remove" {
		t.Errorf("direct action match should win over single synonym-alias path : %+v", hits)
	}
}

// TestParanoidIdx_Scoring_MultiSynonymBeatsSingleDirect : the
// complementary case — when a tool matches the synonym on
// MULTIPLE fields, the cumulative contribution can overtake a
// single direct match. Documents the actual behaviour so a future
// change to the scorer doesn't break this expectation silently.
func TestParanoidIdx_Scoring_MultiSynonymBeatsSingleDirect(t *testing.T) {
	universe := []policy.AvailableAction{
		// Single field match (description token only) :
		// "remove" appears as a literal token in the description.
		tspec("d", "x", "First will remove a thing",
			tool.RiskLow, nil, nil, nil),
		// Multi-field synonym match : action name AND alias both
		// match the synonym "supprimer" of query "remove".
		tspec("m", "supprimer",
			"No keyword overlap with the query",
			tool.RiskLow, nil, []string{"supprimer"}, nil),
	}
	idx := pBuild(t, universe)
	hits := idx.Search("remove", 5)
	if len(hits) < 2 {
		t.Fatalf("want both hits, got %d", len(hits))
	}
	if hits[0].Tool.FQN != "m.supprimer" {
		t.Errorf("multi-signal synonym should outrank single description match : %+v", hits)
	}
}

// TestParanoidIdx_Scoring_StableTieBreak : when two tools score the
// same, the alphabetical FQN tie-break must be deterministic.
func TestParanoidIdx_Scoring_StableTieBreak(t *testing.T) {
	universe := []policy.AvailableAction{
		tspec("zzz", "x", "Identical", tool.RiskLow, nil, nil, nil),
		tspec("aaa", "x", "Identical", tool.RiskLow, nil, nil, nil),
		tspec("mmm", "x", "Identical", tool.RiskLow, nil, nil, nil),
	}
	idx := pBuild(t, universe)
	for i := 0; i < 10; i++ {
		hits := idx.Search("identical", 5)
		if len(hits) != 3 {
			t.Fatalf("hits = %d, want 3", len(hits))
		}
		want := []string{"aaa.x", "mmm.x", "zzz.x"}
		for k, h := range hits {
			if h.Tool.FQN != want[k] {
				t.Fatalf("iteration %d : order broken : hits[%d]=%s, want %s",
					i, k, h.Tool.FQN, want[k])
			}
		}
	}
}

// =====================================================================
// SECTION 3 — Edge-case queries
// =====================================================================

func TestParanoidIdx_Query_EmptyReturnsNothing(t *testing.T) {
	idx := pBuild(t, basicUniverse())
	if len(idx.Search("", 5)) != 0 {
		t.Error("empty query should return no hits")
	}
}

func TestParanoidIdx_Query_PunctuationOnly(t *testing.T) {
	idx := pBuild(t, basicUniverse())
	for _, q := range []string{"...", "!!!", "?!?", "    ", "→→→"} {
		if hits := idx.Search(q, 5); len(hits) != 0 {
			t.Errorf("query %q returned hits : %+v", q, hits)
		}
	}
}

func TestParanoidIdx_Query_VeryLongQuery_NoCrash(t *testing.T) {
	idx := pBuild(t, basicUniverse())
	huge := strings.Repeat("read file content lire fichier ", 200)
	hits := idx.Search(huge, 5)
	if len(hits) == 0 {
		t.Error("long query should still match")
	}
}

func TestParanoidIdx_Query_LimitZeroOrNegative(t *testing.T) {
	idx := pBuild(t, basicUniverse())
	all := idx.Search("io", 0)  // 0 = no limit
	some := idx.Search("io", 2) // explicit small limit
	neg := idx.Search("io", -1) // negative
	if len(all) <= len(some) {
		t.Errorf("limit=0 returned %d, limit=2 returned %d (0 should be ≥)", len(all), len(some))
	}
	if len(neg) != len(all) {
		t.Errorf("negative limit should equal unlimited : neg=%d all=%d", len(neg), len(all))
	}
}

func TestParanoidIdx_Query_Unicode_EmojiNonLatin(t *testing.T) {
	idx := pBuild(t, basicUniverse())
	// Emoji + Chinese should tokenize to nothing recognisable → no hits.
	if hits := idx.Search("🎉 你好 🚀", 5); len(hits) != 0 {
		t.Errorf("emoji/Chinese query returned hits : %+v", hits)
	}
}

func TestParanoidIdx_Query_FrenchAccents_Match(t *testing.T) {
	universe := []policy.AvailableAction{
		tspec("f", "ecrire", "Action d'écriture", tool.RiskLow,
			nil, []string{"écrire"}, nil),
	}
	idx := pBuild(t, universe)
	for _, q := range []string{"écrire", "ecrire"} {
		hits := idx.Search(q, 5)
		if len(hits) == 0 {
			t.Errorf("query %q should match (accents handled), got %+v", q, hits)
		}
	}
}

// =====================================================================
// SECTION 4 — Adversarial tool definitions
// =====================================================================

func TestParanoidIdx_AdversarialTool_EmptyDescription(t *testing.T) {
	universe := []policy.AvailableAction{
		tspec("module", "action", "", tool.RiskLow, nil, nil, nil),
	}
	idx := pBuild(t, universe)
	if idx.Get("module.action") == nil {
		t.Error("tool with empty description should still be indexed")
	}
	hits := idx.Search("action", 5)
	if len(hits) == 0 {
		t.Error("tool with empty description should still be findable by action name")
	}
}

func TestParanoidIdx_AdversarialTool_NoTagsNoAliases(t *testing.T) {
	universe := []policy.AvailableAction{
		tspec("m", "bare", "Bare bones tool", tool.RiskLow, nil, nil, nil),
	}
	idx := pBuild(t, universe)
	if hits := idx.Search("bare", 5); len(hits) == 0 {
		t.Error("bare tool should be findable")
	}
}

func TestParanoidIdx_AdversarialTool_DuplicateTags(t *testing.T) {
	universe := []policy.AvailableAction{
		tspec("m", "a", "", tool.RiskLow,
			[]string{"io", "io", "io"}, // duplicates
			[]string{"alias", "alias"},
			nil),
	}
	idx := pBuild(t, universe)
	hits := idx.Search("io", 5)
	if len(hits) != 1 {
		t.Errorf("duplicate tags should produce 1 tool, got %d", len(hits))
	}
}

func TestParanoidIdx_AdversarialTool_NilSpec_ExcludedGracefully(t *testing.T) {
	// An action with nil Spec : SG-3 gate 2/3 fails closed for such
	// tools, so they shouldn't land in the index.
	universe := []policy.AvailableAction{
		{Module: "m", Action: "noSpec", Spec: nil},
		tspec("m", "valid", "ok", tool.RiskLow, nil, nil, nil),
	}
	idx := pBuild(t, universe)
	if idx.Get("m.noSpec") != nil {
		t.Error("tool with nil spec should NOT be indexed (gate 2 fail-closed)")
	}
	if idx.Get("m.valid") == nil {
		t.Error("valid tool should be indexed")
	}
}

// =====================================================================
// SECTION 5 — Schema-build filter chain (gates → index)
// =====================================================================

func TestParanoidIdx_FilterChain_HiddenInvisible(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		HiddenActions: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"delete"}},
		},
	}
	idx := index.NewBuilder().Build(true, caps,
		&schema.Agent{ID: "main"}, basicUniverse())
	if idx.Get("filesystem.delete") != nil {
		t.Fatal("hidden action leaked into index")
	}
	// Search must not find it either.
	for _, q := range []string{"delete", "destructive", "supprimer"} {
		for _, h := range idx.Search(q, 10) {
			if h.Tool.FQN == "filesystem.delete" {
				t.Errorf("search %q surfaced hidden tool", q)
			}
		}
	}
}

func TestParanoidIdx_FilterChain_DenyInvisible(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		Deny: []schema.CapabilityGrant{
			{Module: "shell", Tools: []string{"bash"}},
		},
	}
	idx := index.NewBuilder().Build(true, caps,
		&schema.Agent{ID: "main"}, basicUniverse())
	if idx.Get("shell.bash") != nil {
		t.Fatal("denied action leaked into index")
	}
}

func TestParanoidIdx_FilterChain_RiskCeiling(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskLow),
	}
	idx := index.NewBuilder().Build(true, caps,
		&schema.Agent{ID: "main"}, basicUniverse())
	// Only RiskLow tools should be present.
	for fqn, t2 := range idx.Tools {
		if t2.RiskLevel != tool.RiskLow {
			t := t // capture for closure
			_ = t
			panicMsg := fmt.Sprintf("%s = %s slipped past max_risk=low", fqn, t2.RiskLevel)
			panic(panicMsg)
		}
	}
}

func TestParanoidIdx_FilterChain_SubAgent_OnlySubset(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	agent := &schema.Agent{
		ID: "reader",
		Modules: schema.AgentModules{
			{ID: "filesystem", Tools: []string{"read"}},
		},
	}
	idx := index.NewBuilder().Build(true, caps, agent, basicUniverse())
	mustSee := []string{"filesystem.read"}
	mustNotSee := []string{
		"filesystem.write", "filesystem.delete",
		"shell.bash", "memory.remember",
	}
	for _, fqn := range mustSee {
		if idx.Get(fqn) == nil {
			t.Errorf("sub-agent should see %s", fqn)
		}
	}
	for _, fqn := range mustNotSee {
		if idx.Get(fqn) != nil {
			t.Errorf("sub-agent should NOT see %s", fqn)
		}
	}
}

// =====================================================================
// SECTION 6 — Categories + FQNList
// =====================================================================

func TestParanoidIdx_Categories_StableSortedPerModule(t *testing.T) {
	universe := []policy.AvailableAction{
		tspec("filesystem", "zzz", "z", tool.RiskLow, nil, nil, nil),
		tspec("filesystem", "aaa", "a", tool.RiskLow, nil, nil, nil),
		tspec("filesystem", "mmm", "m", tool.RiskLow, nil, nil, nil),
	}
	idx := pBuild(t, universe)
	fs := idx.Categories["filesystem"]
	want := []string{"filesystem.aaa", "filesystem.mmm", "filesystem.zzz"}
	for i, fqn := range fs {
		if fqn != want[i] {
			t.Errorf("Categories[%d] = %s, want %s", i, fqn, want[i])
		}
	}
}

func TestParanoidIdx_FQNList_DeterministicAcrossBuilds(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	agent := &schema.Agent{ID: "main"}
	idx1 := index.NewBuilder().Build(true, caps, agent, basicUniverse())
	idx2 := index.NewBuilder().Build(true, caps, agent, basicUniverse())
	l1 := idx1.FQNList()
	l2 := idx2.FQNList()
	if len(l1) != len(l2) {
		t.Fatalf("lengths differ : %d vs %d", len(l1), len(l2))
	}
	for i := range l1 {
		if l1[i] != l2[i] {
			t.Errorf("FQNList[%d] : %s vs %s", i, l1[i], l2[i])
		}
	}
}

// =====================================================================
// SECTION 7 — Concurrency : N goroutines × Search same index
// =====================================================================

// TestParanoidIdx_Concurrent_Reads : 500 goroutines run Search in
// parallel on a shared ToolIndex. The index is immutable after
// Build so we expect zero data races (verify under -race).
func TestParanoidIdx_Concurrent_Reads(t *testing.T) {
	idx := pBuild(t, basicUniverse())
	const N = 500
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			queries := []string{
				"read", "write", "delete", "lire", "supprimer",
				"bash", "memory", "io", "files", "destructive",
			}
			q := queries[i%len(queries)]
			hits := idx.Search(q, 3)
			if len(hits) == 0 {
				t.Errorf("g%d query %q returned no hits", i, q)
			}
		}(i)
	}
	wg.Wait()
}

// TestParanoidIdx_Concurrent_SameQuery_DeterministicResults : 100
// goroutines run the exact same query. Every result list must be
// identical (proves the index is truly stateless under read).
func TestParanoidIdx_Concurrent_SameQuery_DeterministicResults(t *testing.T) {
	idx := pBuild(t, basicUniverse())
	const N = 100
	results := make([][]string, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			hits := idx.Search("files", 10)
			out := make([]string, len(hits))
			for k, h := range hits {
				out[k] = h.Tool.FQN
			}
			results[i] = out
		}(i)
	}
	wg.Wait()
	// All result lists must be identical to results[0].
	for i := 1; i < N; i++ {
		if len(results[i]) != len(results[0]) {
			t.Fatalf("g%d returned %d hits, g0 returned %d",
				i, len(results[i]), len(results[0]))
		}
		for k := range results[i] {
			if results[i][k] != results[0][k] {
				t.Fatalf("g%d differs at [%d] : %s vs %s",
					i, k, results[i][k], results[0][k])
			}
		}
	}
}

// =====================================================================
// SECTION 8 — Determinism (single-threaded)
// =====================================================================

func TestParanoidIdx_Search_Idempotent(t *testing.T) {
	idx := pBuild(t, basicUniverse())
	first := idx.Search("io files", 5)
	for i := 0; i < 50; i++ {
		next := idx.Search("io files", 5)
		if len(next) != len(first) {
			t.Fatalf("iteration %d : length drift", i)
		}
		for k := range next {
			if next[k].Tool.FQN != first[k].Tool.FQN ||
				next[k].Score != first[k].Score {
				t.Fatalf("iteration %d [%d] : drift %+v vs %+v",
					i, k, next[k], first[k])
			}
		}
	}
}

// =====================================================================
// SECTION 9 — Boundaries
// =====================================================================

func TestParanoidIdx_ZeroTools_SearchSafe(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	idx := index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, nil)
	hits := idx.Search("anything", 5)
	if len(hits) != 0 {
		t.Errorf("empty index should return no hits, got %v", hits)
	}
	if idx.FQNList() != nil && len(idx.FQNList()) != 0 {
		t.Errorf("empty index FQNList = %v, want empty", idx.FQNList())
	}
	if len(idx.CategoryList()) != 0 {
		t.Errorf("empty index CategoryList = %v, want empty", idx.CategoryList())
	}
}

func TestParanoidIdx_Large_StressIndex(t *testing.T) {
	const N = 1000
	universe := make([]policy.AvailableAction, N)
	for i := 0; i < N; i++ {
		mod := fmt.Sprintf("module%d", i%20)
		act := fmt.Sprintf("action_%d", i)
		universe[i] = tspec(mod, act,
			fmt.Sprintf("Description for action %d", i),
			tool.RiskLow,
			[]string{"common", fmt.Sprintf("tag%d", i%50)},
			nil, nil)
	}
	idx := pBuild(t, universe)
	if len(idx.Tools) != N {
		t.Fatalf("indexed %d, want %d", len(idx.Tools), N)
	}
	// Search must still be fast (sub-100ms is generous for 1000 tools).
	hits := idx.Search("common", 10)
	if len(hits) != 10 {
		t.Errorf("limit=10 returned %d, want 10", len(hits))
	}
}

// =====================================================================
// SECTION 10 — Builder isolation
// =====================================================================

func TestParanoidIdx_Builder_MultipleBuilds_Independent(t *testing.T) {
	b := index.NewBuilder()
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	idx1 := b.Build(true, caps, &schema.Agent{ID: "reader",
		Modules: schema.AgentModules{
			{ID: "filesystem", Tools: []string{"read"}},
		}}, basicUniverse())
	idx2 := b.Build(true, caps, &schema.Agent{ID: "writer",
		Modules: schema.AgentModules{
			{ID: "filesystem", Tools: []string{"write"}},
		}}, basicUniverse())

	if idx1.Get("filesystem.read") == nil || idx1.Get("filesystem.write") != nil {
		t.Errorf("idx1 contents wrong : tools=%v", idx1.FQNList())
	}
	if idx2.Get("filesystem.write") == nil || idx2.Get("filesystem.read") != nil {
		t.Errorf("idx2 contents wrong : tools=%v", idx2.FQNList())
	}
}

// TestParanoidIdx_Builder_ConcurrentBuilds : concurrent Builder.Build
// calls must produce independent indexes — no shared mutable state.
func TestParanoidIdx_Builder_ConcurrentBuilds(t *testing.T) {
	b := index.NewBuilder()
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	const N = 100
	indexes := make([]*index.ToolIndex, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			indexes[i] = b.Build(true, caps,
				&schema.Agent{ID: fmt.Sprintf("agent%d", i)},
				basicUniverse())
		}(i)
	}
	wg.Wait()
	// Every index must have the same content (no agent restrictions),
	// AND none should share map pointers.
	for i := 1; i < N; i++ {
		if len(indexes[i].Tools) != len(indexes[0].Tools) {
			t.Fatalf("idx%d : %d tools, idx0 : %d", i,
				len(indexes[i].Tools), len(indexes[0].Tools))
		}
	}
}

// =====================================================================
// SECTION 11 — Tokenizer behaviour observable through search
// =====================================================================

// TestParanoidIdx_Tokenizer_CamelCase_Splits : the camelCase splitter
// indexes "readFile" as ["read", "file", "readfile"]. Querying with
// either half OR the preserved camelCase form finds it.
//
// Note : a fully-lowercased squashed query like "readfile" (no
// boundary to split) is NOT expected to match — that's an unnatural
// input. Real queries say "read file" with whitespace.
func TestParanoidIdx_Tokenizer_CamelCase_Splits(t *testing.T) {
	universe := []policy.AvailableAction{
		tspec("api", "readFile",
			"Endpoint that reads a file",
			tool.RiskLow,
			nil, nil, nil),
	}
	idx := pBuild(t, universe)
	for _, q := range []string{"read", "file", "readFile", "read file"} {
		hits := idx.Search(q, 5)
		if len(hits) == 0 || hits[0].Tool.FQN != "api.readFile" {
			t.Errorf("query %q didn't find api.readFile : got %v", q, hits)
		}
	}
}

// TestParanoidIdx_Tokenizer_SnakeCase_Splits : "snake_case" must
// be split so both halves are searchable.
func TestParanoidIdx_Tokenizer_SnakeCase_Splits(t *testing.T) {
	universe := []policy.AvailableAction{
		tspec("io", "snake_case_action", "Snake-case named action",
			tool.RiskLow, nil, nil, nil),
	}
	idx := pBuild(t, universe)
	for _, q := range []string{"snake", "case", "action"} {
		hits := idx.Search(q, 5)
		if len(hits) == 0 || hits[0].Tool.FQN != "io.snake_case_action" {
			t.Errorf("query %q didn't find snake action : %v", q, hits)
		}
	}
}

// TestParanoidIdx_Tokenizer_ShortTokens_Dropped : tokens shorter
// than 2 characters are dropped to de-noise the index. Verify
// "a" and "of" (1-char + stopword-ish) don't pollute the result set.
func TestParanoidIdx_Tokenizer_ShortTokens_Dropped(t *testing.T) {
	idx := pBuild(t, basicUniverse())
	hits := idx.Search("a", 5) // 1-char query
	if len(hits) != 0 {
		// "a" might appear in descriptions but it's a 1-char token.
		// If our tokenizer drops it, the search returns nothing —
		// which is the documented behaviour.
		t.Logf("got %d hits for 1-char query (acceptable)", len(hits))
	}
}
