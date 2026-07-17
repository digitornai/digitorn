package contextcompact

import (
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

func srcMsg(role, text string) sessionstore.Message {
	return sessionstore.Message{Role: role, Content: text}
}

func TestAppendMissingSourceFacts(t *testing.T) {
	dropped := []sessionstore.Message{
		srcMsg("user", "Remember: codeword ORCHID-9, port 8080, file ./data/orchid.db, rate 47 rpm."),
		srcMsg("assistant", "OK."),
		srcMsg("system", "PRIOR RECAP: irrelevant-token ZZZ-99"),
	}

	got := appendMissingSourceFacts("KEY FACTS: ORCHID-9, rate 47.", dropped)
	for _, want := range []string{"8080", "orchid.db"} {
		if !strings.Contains(strings.ToLower(got), want) {
			t.Errorf("backstop did not recover %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ZZZ-99") {
		t.Errorf("backstop wrongly pulled a token from the prior recap: %s", got)
	}
	full := "KEY FACTS: ORCHID-9, port 8080, ./data/orchid.db, 47 rpm."
	if out := appendMissingSourceFacts(full, dropped); out != full {
		t.Errorf("complete summary must be unchanged, got:\n%s", out)
	}
}

func TestSummaryDroppedFact(t *testing.T) {
	prior := `KEY FACTS:
- Codename: ORCHID-9
- Storage: single SQLite file at ` + "`./data/orchid.db`" + ` — Postgres rejected
- Rate limit: 47 requests per minute
- I/O: async via the EventLoop; exporter must output Parquet

MISSION: build ORCHID-9 component by component.`

	cases := []struct {
		name string
		next string
		drop bool
	}{
		{"keeps all facts", prior, false},
		{"drops the number", `KEY FACTS:
- Codename: ORCHID-9
- Storage: ` + "`./data/orchid.db`" + ` (SQLite), Postgres rejected
- I/O: async EventLoop, Parquet exporter`, true},
		{"drops the codename", `KEY FACTS: storage orchid.db (SQLite), Postgres rejected, 47 rpm, EventLoop, Parquet`, true},
		{"drops SQLite", `KEY FACTS: ORCHID-9, orchid.db, 47 rpm, EventLoop, Parquet`, true},
		{"collapsed stub", `**ORCHID-9**`, true},
		{"reordered + reworded but complete", `KEY FACTS: Parquet exporter via EventLoop; 47 req/min cap; orchid.db (SQLite, not Postgres); project ORCHID-9.`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tok, got := summaryDroppedFact(prior, c.next)
			if got != c.drop {
				t.Errorf("summaryDroppedFact = %v (tok=%q), want %v", got, tok, c.drop)
			}
		})
	}
}

func TestExtractNewKeyFacts(t *testing.T) {
	summary := "KEY FACTS:\n- Codename: ORCHID-9\n- Storage: ` ./data/orchid.db ` (SQLite, Postgres rejected)\n- Rate limit: 47 rpm\n- No actionable task yet\n\nMISSION: build ORCHID-9."

	got := ExtractNewKeyFacts(summary, nil)
	joined := strings.ToLower(strings.Join(got, " | "))
	for _, want := range []string{"orchid-9", "orchid.db", "47"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing fact %q in %v", want, got)
		}
	}
	if strings.Contains(joined, "no actionable task") {
		t.Errorf("narrative line (no concrete token) must be skipped: %v", got)
	}
	if again := ExtractNewKeyFacts(summary, got); len(again) != 0 {
		t.Errorf("known facts must not be re-extracted, got %v", again)
	}
	evolved := "KEY FACTS:\n- Codename: ORCHID-9\n- Service port: 9090 (was 8080)"
	ev := ExtractNewKeyFacts(evolved, []string{"Codename: ORCHID-9", "Service port: 8080"})
	if len(ev) == 0 || !strings.Contains(strings.Join(ev, " "), "9090") {
		t.Errorf("evolved value 9090 must be extracted as a new fact, got %v", ev)
	}
}

func TestStripKeyFactsSection(t *testing.T) {
	recap := "Context checkpoint — resume.\n\n<recap>\nKEY FACTS:\n- Codename: ORCHID-9\n- Rate limit: 47 rpm\n\nMISSION: build ORCHID-9 component by component.\nOPEN ITEMS: exporter next.\n</recap>\n\nResume the mission."
	got := StripKeyFactsSection(recap)
	if strings.Contains(got, "ORCHID-9\n- Rate") || strings.Contains(got, "47 rpm") {
		t.Errorf("KEY FACTS not stripped:\n%s", got)
	}
	for _, want := range []string{"MISSION", "exporter next", "Resume the mission", "<recap>"} {
		if !strings.Contains(got, want) {
			t.Errorf("narrative %q lost when stripping facts:\n%s", want, got)
		}
	}
	plain := "Context checkpoint.\n\n<recap>\nMISSION: do the thing.\n</recap>"
	if out := StripKeyFactsSection(plain); out != plain {
		t.Errorf("recap without KEY FACTS must be unchanged, got:\n%s", out)
	}
}

func TestIsStructuredToken(t *testing.T) {
	yes := []string{"47", "ORCHID-9", "EventLoop", "SQLite", "orchid.db", "rate_limiter.py", "2048", "HTTP"}
	no := []string{"the", "storage", "rejected", "component", "a", "OK", "and"}
	for _, w := range yes {
		if !isStructuredToken(w) {
			t.Errorf("isStructuredToken(%q) = false, want true", w)
		}
	}
	for _, w := range no {
		if isStructuredToken(w) {
			t.Errorf("isStructuredToken(%q) = true, want false", w)
		}
	}
}
