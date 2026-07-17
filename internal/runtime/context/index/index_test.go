package index_test

import (
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

func makeTool(module, action, desc string, risk tool.RiskLevel, tags, aliases []string) policy.AvailableAction {
	return policy.AvailableAction{
		Module: module,
		Action: action,
		Spec: &tool.Spec{
			Name:        module + "." + action,
			Description: desc,
			RiskLevel:   risk,
			Tags:        tags,
			Aliases:     aliases,
		},
	}
}

func defaultUniverse() []policy.AvailableAction {
	return []policy.AvailableAction{
		makeTool("filesystem", "read",
			"Read the contents of a file from disk",
			tool.RiskLow,
			[]string{"io", "files"},
			[]string{"lire", "open"}),
		makeTool("filesystem", "write",
			"Write content to a file on disk",
			tool.RiskMedium,
			[]string{"io", "files"},
			[]string{"ecrire", "save"}),
		makeTool("filesystem", "delete",
			"Delete a file from disk permanently",
			tool.RiskHigh,
			[]string{"io", "files", "destructive"},
			[]string{"supprimer", "rm"}),
		makeTool("shell", "bash",
			"Execute a Bash command in a shell",
			tool.RiskHigh,
			[]string{"process", "shell"},
			[]string{"exec", "command"}),
		makeTool("memory", "remember",
			"Store a fact in long-term memory",
			tool.RiskLow,
			[]string{"state", "memory"},
			[]string{"memoriser", "save"}),
		makeTool("http", "post",
			"Send an HTTP POST request to a URL",
			tool.RiskMedium,
			[]string{"network", "web"},
			[]string{"envoyer"}),
	}
}

func build(t *testing.T, universe []policy.AvailableAction) *index.ToolIndex {
	t.Helper()
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	agent := &schema.Agent{ID: "main"}
	return index.NewBuilder().Build(true, caps, agent, universe)
}

func TestFQNListSorted(t *testing.T) {
	idx := build(t, defaultUniverse())
	fqns := idx.FQNList()
	for i := 1; i < len(fqns); i++ {
		if fqns[i-1] >= fqns[i] {
			t.Fatalf("FQNs not sorted : %v", fqns)
		}
	}
}

func TestCategoryListSorted(t *testing.T) {
	idx := build(t, defaultUniverse())
	cats := idx.CategoryList()
	for i := 1; i < len(cats); i++ {
		if cats[i-1] >= cats[i] {
			t.Fatalf("Categories not sorted : %v", cats)
		}
	}
	want := []string{"filesystem", "http", "memory", "shell"}
	if len(cats) != len(want) {
		t.Fatalf("Categories = %v, want %v", cats, want)
	}
	for i := range cats {
		if cats[i] != want[i] {
			t.Fatalf("Categories[%d] = %q, want %q", i, cats[i], want[i])
		}
	}
}

func TestGetByFQN(t *testing.T) {
	idx := build(t, defaultUniverse())
	if got := idx.Get("filesystem.read"); got == nil || got.Action != "read" {
		t.Fatalf("Get(filesystem.read) = %+v", got)
	}
	if got := idx.Get("nonexistent.tool"); got != nil {
		t.Errorf("Get(missing) should return nil, got %+v", got)
	}
}

func TestBuild_HonoursHiddenActions(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		HiddenActions: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"delete"}},
		},
	}
	idx := index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, defaultUniverse())
	if idx.Get("filesystem.delete") != nil {
		t.Fatal("hidden action leaked into the index")
	}
	if idx.Get("filesystem.read") == nil {
		t.Error("non-hidden actions should still appear")
	}
}

func TestBuild_HonoursDeny(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		Deny: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"delete"}},
		},
	}
	idx := index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, defaultUniverse())
	if idx.Get("filesystem.delete") != nil {
		t.Fatal("denied action leaked into the index")
	}
}

func TestBuild_HonoursMaxRisk(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskMedium),
	}
	idx := index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, defaultUniverse())
	if idx.Get("shell.bash") != nil {
		t.Fatal("high-risk shell.bash should be filtered")
	}
	if idx.Get("filesystem.delete") != nil {
		t.Fatal("high-risk filesystem.delete should be filtered")
	}
	if idx.Get("filesystem.read") == nil {
		t.Error("low-risk filesystem.read should remain")
	}
}

func TestBuild_HonoursAgentSubset(t *testing.T) {
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
	idx := index.NewBuilder().Build(true, caps, agent, defaultUniverse())
	if idx.Get("filesystem.read") == nil {
		t.Error("read must be visible")
	}
	if idx.Get("filesystem.write") != nil {
		t.Error("write must be filtered out of sub-agent view")
	}
	if idx.Get("shell.bash") != nil {
		t.Error("shell module not in agent's set")
	}
}

func TestSearch_ExactFQN(t *testing.T) {
	idx := build(t, defaultUniverse())
	hits := idx.Search("filesystem.read", 5)
	if len(hits) == 0 {
		t.Fatal("no hits for exact FQN")
	}
	if hits[0].Tool.FQN != "filesystem.read" {
		t.Errorf("top hit = %s, want filesystem.read", hits[0].Tool.FQN)
	}
}

func TestSearch_ActionName(t *testing.T) {
	idx := build(t, defaultUniverse())
	hits := idx.Search("read", 5)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Tool.FQN != "filesystem.read" {
		t.Errorf("top = %s, want filesystem.read", hits[0].Tool.FQN)
	}
}

// TestSearch_Prefix : "fil" matches the filesystem actions.
func TestSearch_Prefix(t *testing.T) {
	idx := build(t, defaultUniverse())
	hits := idx.Search("fil", 10)
	hitNames := map[string]bool{}
	for _, h := range hits {
		hitNames[h.Tool.FQN] = true
	}
	for _, want := range []string{"filesystem.read", "filesystem.write", "filesystem.delete"} {
		if !hitNames[want] {
			t.Errorf("%s should appear in prefix hits, got %v", want, hits)
		}
	}
}

// TestSearch_Tag : a tag-based query lands the tagged tools.
func TestSearch_Tag(t *testing.T) {
	idx := build(t, defaultUniverse())
	hits := idx.Search("destructive", 5)
	if len(hits) == 0 || hits[0].Tool.FQN != "filesystem.delete" {
		t.Errorf("destructive tag should point to filesystem.delete, got %+v", hits)
	}
}

// TestSearch_AliasFR : the FR alias "supprimer" finds filesystem.delete.
func TestSearch_AliasFR(t *testing.T) {
	idx := build(t, defaultUniverse())
	hits := idx.Search("supprimer", 5)
	if len(hits) == 0 || hits[0].Tool.FQN != "filesystem.delete" {
		t.Errorf("supprimer should find filesystem.delete, got %+v", hits)
	}
}

// TestSearch_Synonym : "remove" (synonym of delete) finds filesystem.delete.
// "remove" isn't directly indexed (no alias), but the synonym table
// expands the query so the delete-bucket matches.
func TestSearch_Synonym(t *testing.T) {
	idx := build(t, defaultUniverse())
	hits := idx.Search("remove", 5)
	found := false
	for _, h := range hits {
		if h.Tool.FQN == "filesystem.delete" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("synonym 'remove' should find filesystem.delete, got %+v", hits)
	}
}

// TestSearch_FRSynonym : "écrire" expands to "write" → finds
// filesystem.write. Verifies accents are handled.
func TestSearch_FRSynonym(t *testing.T) {
	idx := build(t, defaultUniverse())
	hits := idx.Search("écrire", 5)
	found := false
	for _, h := range hits {
		if h.Tool.FQN == "filesystem.write" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("French 'écrire' should find filesystem.write, got %+v", hits)
	}
}

// TestSearch_DescriptionToken : the word "Bash" in shell.bash's
// description should be findable.
func TestSearch_DescriptionToken(t *testing.T) {
	idx := build(t, defaultUniverse())
	hits := idx.Search("bash", 5)
	if len(hits) == 0 || hits[0].Tool.FQN != "shell.bash" {
		t.Errorf("query 'bash' should top with shell.bash, got %+v", hits)
	}
}

// TestSearch_NoMatch : a totally unrelated query returns no hits.
func TestSearch_NoMatch(t *testing.T) {
	idx := build(t, defaultUniverse())
	hits := idx.Search("xyzzy-nonexistent-zzz", 5)
	if len(hits) != 0 {
		t.Errorf("unrelated query should return no hits, got %+v", hits)
	}
}

// TestSearch_Limit : the limit parameter is enforced.
func TestSearch_Limit(t *testing.T) {
	idx := build(t, defaultUniverse())
	hits := idx.Search("io", 2) // tag matches read/write/delete
	if len(hits) != 2 {
		t.Errorf("limit not enforced : got %d hits, want 2", len(hits))
	}
}

// TestSearch_Ranking_ExactBeatsPrefix : an exact action-name match
// must rank above a prefix-only match.
func TestSearch_Ranking_ExactBeatsPrefix(t *testing.T) {
	universe := []policy.AvailableAction{
		makeTool("readable", "x", "long description", tool.RiskLow, nil, nil),
		makeTool("filesystem", "read", "read a file", tool.RiskLow, nil, nil),
	}
	idx := build(t, universe)
	hits := idx.Search("read", 5)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Tool.FQN != "filesystem.read" {
		t.Errorf("exact match should rank first, got %s", hits[0].Tool.FQN)
	}
}

// TestSearch_EmptyQuery : empty query returns no hits.
func TestSearch_EmptyQuery(t *testing.T) {
	idx := build(t, defaultUniverse())
	if hits := idx.Search("", 5); len(hits) != 0 {
		t.Errorf("empty query returned hits : %+v", hits)
	}
}

// TestSearch_PunctuationOnly_NoHits : "...!!!" tokenizes to nothing.
func TestSearch_PunctuationOnly_NoHits(t *testing.T) {
	idx := build(t, defaultUniverse())
	if hits := idx.Search("...!!!", 5); len(hits) != 0 {
		t.Errorf("punctuation-only query returned hits : %+v", hits)
	}
}

// ---- Synonym table tests -------------------------------------------

func TestSynonyms_Expand(t *testing.T) {
	tbl := index.NewSynonymTable([][]string{
		{"delete", "remove", "destroy"},
		{"read", "lire"},
	})
	got := tbl.Expand("delete")
	want := map[string]bool{"delete": true, "remove": true, "destroy": true}
	if len(got) != 3 {
		t.Fatalf("Expand(delete) = %v, want 3 words", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("Expand(delete) contains unexpected %q", g)
		}
	}
}

func TestSynonyms_ExpandUnknown(t *testing.T) {
	tbl := index.NewSynonymTable([][]string{
		{"delete", "remove"},
	})
	got := tbl.Expand("xyz")
	if len(got) != 1 || got[0] != "xyz" {
		t.Errorf("Expand of unknown should return [xyz], got %v", got)
	}
}

func TestSynonyms_Includes_Symmetric(t *testing.T) {
	tbl := index.NewSynonymTable([][]string{
		{"delete", "remove"},
	})
	if !tbl.Includes("delete", "remove") {
		t.Error("delete/remove should be synonyms")
	}
	if !tbl.Includes("remove", "delete") {
		t.Error("symmetry broken")
	}
	if tbl.Includes("delete", "read") {
		t.Error("unrelated words should not match")
	}
	if !tbl.Includes("read", "read") {
		t.Error("a word is always its own synonym")
	}
}

func TestSynonyms_NilSafe(t *testing.T) {
	var tbl *index.SynonymTable
	got := tbl.Expand("anything")
	if len(got) != 1 || got[0] != "anything" {
		t.Errorf("nil Expand = %v, want [anything]", got)
	}
	if tbl.Includes("a", "b") {
		t.Error("nil Includes should be false")
	}
	if tbl.Size() != 0 {
		t.Error("nil Size should be 0")
	}
}

func TestSynonyms_DefaultLoaded(t *testing.T) {
	tbl := index.DefaultSynonyms()
	if tbl.Size() < 10 {
		t.Errorf("default table size = %d, want >= 10 buckets", tbl.Size())
	}
	if !tbl.Includes("delete", "supprimer") {
		t.Error("default table should contain delete/supprimer")
	}
	if !tbl.Includes("read", "lire") {
		t.Error("default table should contain read/lire")
	}
}
