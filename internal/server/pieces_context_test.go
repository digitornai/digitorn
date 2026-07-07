package server

import (
	"strings"
	"testing"

	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/runtime/policy"
	"github.com/digitornai/digitorn/pkg/module"
)

func TestPiecesConnectorSection_EmptyReturnsNil(t *testing.T) {
	if sec := piecesConnectorSection(nil); sec != nil {
		t.Fatalf("no actions must yield nil section, got %+v", sec)
	}
	if sec := piecesConnectorSection([]policy.AvailableAction{}); sec != nil {
		t.Fatalf("empty actions must yield nil section, got %+v", sec)
	}
}

func TestPiecesConnectorSection_GroupsSortsCapsAndFrames(t *testing.T) {
	actions := []policy.AvailableAction{
		{Module: "ap_slack", Action: "send_message"},
		{Module: "ap_github", Action: "create_issue"},
		{Module: "ap_github", Action: "list_repos"},
		{Module: "ap_github", Action: "get_pr"},
		{Module: "ap_github", Action: "merge_pr"},
		{Module: "ap_github", Action: "add_comment"},
		{Module: "ap_github", Action: "close_issue"},
		{Module: "ap_github", Action: "seventh_over_cap"},
		{Module: "ap_slack", Action: "list_channels"},
	}
	sec := piecesConnectorSection(actions)
	if sec == nil {
		t.Fatal("expected a section")
	}
	if sec.Title != "Connected connectors" {
		t.Fatalf("title = %q", sec.Title)
	}
	c := sec.Content

	if !strings.Contains(c, "Authentication is handled automatically") ||
		!strings.Contains(c, "never ask the user for an API key") {
		t.Errorf("auth guidance missing:\n%s", c)
	}
	if !strings.Contains(c, "- github (ap_github__*)") {
		t.Errorf("github line missing:\n%s", c)
	}
	if !strings.Contains(c, "- slack (ap_slack__*)") {
		t.Errorf("slack line missing:\n%s", c)
	}
	if strings.Index(c, "- github") > strings.Index(c, "- slack") {
		t.Errorf("connectors must be alphabetically sorted:\n%s", c)
	}
	if strings.Contains(c, "seventh_over_cap") {
		t.Errorf("example actions must cap at 6 per connector:\n%s", c)
	}
	if !strings.Contains(c, "search_tools") || !strings.Contains(c, "Settings → Connectors") {
		t.Errorf("usage/recovery guidance missing:\n%s", c)
	}
}

func TestPiecesConnectorSection_WildcardCapsAndSummarizes(t *testing.T) {
	actions := make([]policy.AvailableAction, 0, 200)
	for i := 0; i < 200; i++ {
		actions = append(actions, policy.AvailableAction{
			Module:        "ap_conn" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Action:        "do",
			DiscoveryOnly: true,
		})
	}
	sec := piecesConnectorSection(actions)
	if sec == nil {
		t.Fatal("expected a section")
	}
	c := sec.Content
	lines := strings.Count(c, "\n- ")
	if lines > piecesSectionMaxConnectors+1 {
		t.Errorf("wildcard section must cap enumerated connectors near %d, got %d bullet lines", piecesSectionMaxConnectors, lines)
	}
	if !strings.Contains(c, "more — use search_tools") {
		t.Errorf("truncated wildcard section must point to search_tools:\n%s", c)
	}
	if !strings.Contains(c, "can reach 200 connectors") {
		t.Errorf("wildcard intro must state the total:\n%s", c)
	}
}

func TestGather_PiecesSection_AuthorizedGatedAndAppScoped(t *testing.T) {
	const appID = "github-agent"
	pc := newPiecesCatalog(module.NewRegistry(), nil)
	pc.byApp[appID] = []policy.AvailableAction{
		{Module: "ap_github", Action: "create_issue"},
		{Module: "ap_github", Action: "list_repos"},
	}
	c := registryContributors{Registry: module.NewRegistry(), Pieces: pc}

	has := func(secs []domainmodule.PromptSection) bool {
		for _, s := range secs {
			if s.Title == "Connected connectors" {
				return true
			}
		}
		return false
	}

	secs, _ := c.Gather(domainmodule.PromptScope{AppID: appID}, []string{"pieces"})
	if !has(secs) {
		t.Errorf("pieces authorized + app-scoped must emit the connectors section, got %+v", secs)
	}

	secs, _ = c.Gather(domainmodule.PromptScope{AppID: appID}, []string{"filesystem"})
	if has(secs) {
		t.Errorf("anti-leak: agent without pieces authorization must NOT see the connectors section")
	}

	secs, _ = c.Gather(domainmodule.PromptScope{AppID: ""}, []string{"pieces"})
	if has(secs) {
		t.Errorf("no app scope must NOT emit a connectors section")
	}
}
