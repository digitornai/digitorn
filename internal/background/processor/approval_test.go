package processor

import (
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/background/adapter"
	"github.com/digitornai/digitorn/internal/background/daemonclient"
)

func TestBuildPromptRequest_ToolCall(t *testing.T) {
	ap := daemonclient.Approval{
		Kind: "tool_call", ToolName: "shell.bash", RiskLevel: "high",
		Reason: "shell needs approval", ToolParams: map[string]any{"command": "rm -rf /tmp/x"},
	}
	req, ok := buildPromptRequest(map[string]any{"channel_id": "c1"}, ap)
	if !ok {
		t.Fatal("tool_call must be expressible")
	}
	if len(req.Options) != 2 || req.Options[0].ID != "grant" || req.Options[1].ID != "deny" {
		t.Fatalf("tool_call must offer grant/deny, got %+v", req.Options)
	}
	if req.AllowText {
		t.Fatal("tool_call must not offer free text")
	}
	for _, want := range []string{"shell.bash", "high", "rm -rf"} {
		if !strings.Contains(req.Body, want) {
			t.Fatalf("body missing %q: %s", want, req.Body)
		}
	}
}

func TestBuildPromptRequest_QuestionChoices(t *testing.T) {
	ap := daemonclient.Approval{
		Kind:   "question",
		Reason: "Quelle base ?",
		Payload: map[string]any{
			"choices":      []any{"postgres", "mysql"},
			"allow_custom": true,
		},
	}
	req, ok := buildPromptRequest(map[string]any{"channel_id": "c1"}, ap)
	if !ok {
		t.Fatal("single-choice question must be expressible")
	}
	if len(req.Options) != 2 || req.Options[0].ID != "postgres" || req.Options[1].ID != "mysql" {
		t.Fatalf("choices must map to options, got %+v", req.Options)
	}
	if !req.AllowText {
		t.Fatal("allow_custom must enable free text")
	}
}

func TestBuildPromptRequest_QuestionFreeText(t *testing.T) {
	ap := daemonclient.Approval{Kind: "question", Reason: "Ton nom ?"}
	req, ok := buildPromptRequest(map[string]any{"channel_id": "c1"}, ap)
	if !ok {
		t.Fatal("free-text question must be expressible")
	}
	if len(req.Options) != 0 || !req.AllowText {
		t.Fatalf("no-choice question must be free text only, got %+v allowText=%v", req.Options, req.AllowText)
	}
}

func TestBuildPromptRequest_DegradesMultiSelectAndForm(t *testing.T) {
	multi := daemonclient.Approval{Kind: "question", Reason: "?", Payload: map[string]any{
		"choices": []any{"a", "b"}, "allow_multiple": true}}
	if _, ok := buildPromptRequest(nil, multi); ok {
		t.Fatal("multi-select must degrade (ok=false)")
	}
	form := daemonclient.Approval{Kind: "question", Reason: "?", Payload: map[string]any{
		"form": []any{map[string]any{"name": "x"}}}}
	if _, ok := buildPromptRequest(nil, form); ok {
		t.Fatal("form must degrade (ok=false)")
	}
}

func TestMapPromptResponse(t *testing.T) {
	tool := daemonclient.Approval{Kind: "tool_call"}
	if a, _ := mapPromptResponse(tool, adapter.PromptResponse{OptionID: "grant"}); a != "grant" {
		t.Fatalf("tool grant → action grant, got %q", a)
	}
	if a, _ := mapPromptResponse(tool, adapter.PromptResponse{OptionID: "deny"}); a != "deny" {
		t.Fatalf("tool deny → action deny, got %q", a)
	}
	q := daemonclient.Approval{Kind: "question"}
	if a, r := mapPromptResponse(q, adapter.PromptResponse{OptionID: "postgres"}); a != "grant" || r != "postgres" {
		t.Fatalf("question choice → grant+choice, got %q/%q", a, r)
	}
	if a, r := mapPromptResponse(q, adapter.PromptResponse{Text: "ma réponse"}); a != "grant" || r != "ma réponse" {
		t.Fatalf("free text → grant+text, got %q/%q", a, r)
	}
}
