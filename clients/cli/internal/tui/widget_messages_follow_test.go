package tui

import (
	"fmt"
	"testing"

	"github.com/digitornai/digitorn-cli/internal/client"
	"github.com/digitornai/digitorn-cli/internal/theme"
)

func manyMsgs(n int) []client.Message {
	out := make([]client.Message, n)
	for i := range out {
		out[i] = client.Message{Role: "assistant", Content: fmt.Sprintf("message line number %d", i)}
	}
	return out
}

// Sticky follow : while ATTACHED (the default), new content keeps the view
// pinned to the bottom — this is what keeps a long-running tool's chip visible
// as it works. Once the USER scrolls away (detached), new content must NOT yank
// them back. An explicit GotoBottom re-attaches.
func TestMessages_StickyFollow(t *testing.T) {
	m := NewMessages(theme.Default())
	m.SetSize(40, 5)      // content will exceed the 5-line viewport

	// Attached by default : SetMessages auto-follows to the bottom.
	m.SetMessages(manyMsgs(30))
	if !m.AtBottom() {
		t.Fatal("attached: should auto-follow to the bottom")
	}

	// More content while attached keeps us pinned (e.g. a running tool chip).
	m.SetMessages(manyMsgs(40))
	if !m.AtBottom() {
		t.Fatal("attached: new content must keep the view pinned to the bottom")
	}

	// Simulate the user scrolling away from the bottom.
	m.detached = true
	m.SetMessages(manyMsgs(50))
	if m.AtBottom() {
		t.Fatal("detached: new content must NOT yank the view back to the bottom")
	}

	// An explicit GotoBottom re-attaches : follow resumes.
	m.GotoBottom()
	if m.detached {
		t.Fatal("GotoBottom must re-attach (clear detached)")
	}
	m.SetMessages(manyMsgs(60))
	if !m.AtBottom() {
		t.Fatal("re-attached: new content must follow to the bottom again")
	}
}
