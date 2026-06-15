package runtime

import "testing"

func TestSplitInlineReasoning(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantClean string
		wantReas  string
	}{
		{
			name:      "no tags is byte-identical",
			in:        "Voici les 3 fichiers du dossier : a.go, b.go, go.mod.",
			wantClean: "Voici les 3 fichiers du dossier : a.go, b.go, go.mod.",
			wantReas:  "",
		},
		{
			name:      "balanced leading block",
			in:        "<think>Let me list the files first.</think>The folder has 3 files.",
			wantClean: "The folder has 3 files.",
			wantReas:  "Let me list the files first.",
		},
		{
			// The exact failure from the live log: Kimi leaked its reasoning (in
			// Chinese) with a dangling </think> before the real answer.
			name:      "dangling close with leaked CJK reasoning (the live kimi bug)",
			in:        "思考问题：用户没有提供具体请求。让我先检查一下工作目录中有什么。 </think> I'll check the workspace to see if there's anything to work with.",
			wantClean: "I'll check the workspace to see if there's anything to work with.",
			wantReas:  "思考问题：用户没有提供具体请求。让我先检查一下工作目录中有什么。",
		},
		{
			name:      "dangling open (truncated mid-thought) yields empty answer",
			in:        "<think>I should read each file and summarize but I got cut off",
			wantClean: "",
			wantReas:  "I should read each file and summarize but I got cut off",
		},
		{
			name:      "multiline thinking variant",
			in:        "<thinking>\nstep 1\nstep 2\n</thinking>\nDone.",
			wantClean: "Done.",
			wantReas:  "step 1\nstep 2",
		},
		{
			name:      "multiple balanced blocks both captured",
			in:        "<think>a</think>Answer part 1. <think>b</think>",
			wantClean: "Answer part 1.",
			wantReas:  "a\nb",
		},
		{
			name:      "only reasoning leaves an empty answer (channel skips empty)",
			in:        "<think>just thinking, no answer</think>",
			wantClean: "",
			wantReas:  "just thinking, no answer",
		},
		{
			name:      "case-insensitive tags",
			in:        "<THINK>upper</THINK>final",
			wantClean: "final",
			wantReas:  "upper",
		},
		{
			name:      "empty content",
			in:        "",
			wantClean: "",
			wantReas:  "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clean, reas := splitInlineReasoning(c.in)
			if clean != c.wantClean {
				t.Errorf("clean:\n  got  %q\n  want %q", clean, c.wantClean)
			}
			if reas != c.wantReas {
				t.Errorf("reasoning:\n  got  %q\n  want %q", reas, c.wantReas)
			}
		})
	}
}

// TestSplitInlineReasoning_NeverDropsRealAnswer guards against the worst failure
// mode: silently swallowing a user-facing answer. Any content without think tags
// must round-trip exactly, and any content WITH tags must still keep the trailing
// answer text.
func TestSplitInlineReasoning_NeverDropsRealAnswer(t *testing.T) {
	answers := []string{
		"Plain answer.",
		"A line about <html> tags but not think tags.",
		"Code: if x < y { return }",
		"Discussion of </close> style tags inline.",
	}
	for _, a := range answers {
		if clean, reas := splitInlineReasoning(a); clean != a || reas != "" {
			t.Errorf("non-think content altered: in=%q clean=%q reas=%q", a, clean, reas)
		}
	}
}
