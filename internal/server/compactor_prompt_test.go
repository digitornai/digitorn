package server

import (
	"fmt"
	"strings"
	"testing"
)

// The summarizer prompt must communicate its length budget to the model so it
// emits a COMPLETE handoff that fits — instead of running free and getting cut
// off mid-sentence by the MaxTokens cap (which would eat the last, most useful
// sections). The budget scales with summary_max_tokens and is floored.

func TestBuildSummarizerPrompt_StatesScaledBudget(t *testing.T) {
	for _, tc := range []struct {
		maxTokens int
		wantWords int
	}{
		{maxTokens: 2048, wantWords: 1433}, // 2048*7/10
		{maxTokens: 400, wantWords: 280},   // small app cap
		{maxTokens: 100, wantWords: 80},    // below floor → floored at 80
		{maxTokens: 0, wantWords: 80},      // unset → floored
	} {
		p := buildSummarizerPrompt(tc.maxTokens)
		if !strings.Contains(p, fmt.Sprintf("about %d words", tc.wantWords)) {
			t.Errorf("maxTokens=%d: prompt missing %q\n%s", tc.maxTokens, fmt.Sprintf("about %d words", tc.wantWords), p)
		}
	}
}

func TestBuildSummarizerPrompt_KeepsStructureAndPriority(t *testing.T) {
	p := buildSummarizerPrompt(2048)
	for _, want := range []string{
		"KEY FACTS:", "MISSION:", "TASK & PLAN:", "PROGRESS:", "FILES & ARTIFACTS:", "OPEN ITEMS:", "PITFALLS:",
		"LENGTH BUDGET:", "never cut off mid-sentence",
		"VERBATIM",
		"MISSION, OPEN ITEMS and the immediate next step",
		"MERGE it with the newer messages",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
