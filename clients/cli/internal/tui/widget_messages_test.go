package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn-cli/internal/client"
	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

// stripANSI removes SGR colour codes so tests can match the visible
// text. Themed glamour wraps each word in its own colour span, which
// would otherwise split substrings like "Hello!" across escape codes.
func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func TestMessages_CommittedAssistantRenders(t *testing.T) {
	m := NewMessages(theme.Default())
	m.SetSize(80, 20)
	m.SetMessages([]client.Message{
		{Role: "user", Content: "hi", Seq: 1},
		{Role: "assistant", Content: "Hello there", Seq: 2},
	})
	v := m.View()
	if !strings.Contains(v, "Hello") {
		t.Fatalf("committed assistant message not visible in view:\n%q", v)
	}
}

func TestMessages_StreamingThenFinal(t *testing.T) {
	m := NewMessages(theme.Default())
	m.SetSize(80, 20)
	m.SetMessages([]client.Message{{Role: "user", Content: "hi", Seq: 1}})

	// Streaming preview shows the partial reply live. The markdown render is
	// coalesced to the animation tick, so flush it with RefreshRunning (what the
	// tick calls) before asserting on the view.
	m.SetStreaming("Hel")
	m.RefreshRunning()
	if v := m.View(); !strings.Contains(stripANSI(v), "Hel") {
		t.Fatalf("streaming preview not visible:\n%q", v)
	}

	// Final message commits, stream cleared : final visible.
	m.SetMessages([]client.Message{
		{Role: "user", Content: "hi", Seq: 1},
		{Role: "assistant", Content: "Hello!", Seq: 2},
	})
	m.SetStreaming("")
	if v := m.View(); !strings.Contains(stripANSI(v), "Hello!") {
		t.Fatalf("final assistant message not visible after stream cleared:\n%q", v)
	}
}
