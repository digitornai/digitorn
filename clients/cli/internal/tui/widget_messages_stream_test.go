package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn-cli/internal/theme"
)

// The streaming-tool line must carry the SAME verb the real chip uses (so the
// two look identical and the transition is seamless) : "$" for a shell tool,
// "Read" for a read. If toolVerb ever drifts from the chip, this catches it.
func TestStreamingToolLine_UsesChipVerb(t *testing.T) {
	m := NewMessages(theme.Default())
	m.SetSize(60, 10)

	if got := m.StreamingToolLine("shell.bash", 0); !strings.Contains(got, "$") {
		t.Fatalf("shell tool streaming line should show the $ verb: %q", got)
	}
	if got := m.StreamingToolLine("filesystem.read", 0); !strings.Contains(got, "Read") {
		t.Fatalf("read tool streaming line should show the Read verb: %q", got)
	}
	// Still streaming → the unknown-argument ellipsis, never a duration/caret.
	line := m.StreamingToolLine("filesystem.read", 0)
	if !strings.Contains(line, "…") || strings.Contains(line, "▸") {
		t.Fatalf("streaming line should end in … and carry no caret: %q", line)
	}
	// No per-line token count — that lives only on the central working indicator.
	if strings.Contains(line, "tok") {
		t.Fatalf("streaming line must NOT show a per-line token count: %q", line)
	}
}

// renderStreaming must REUSE its last glamour render within the adaptive
// cooldown (so a long reply can't re-glamour the whole buffer every frame and
// saturate the loop), and RECOMPUTE once the cooldown elapses. This locks in
// the streaming-perf fix.
func TestRenderStreaming_AdaptiveCacheReuseThenRecompute(t *testing.T) {
	m := &Messages{theme: theme.Default(), width: 80}

	first := m.renderStreaming("# Title\n\nfirst body", 80)
	if first == "" {
		t.Fatal("first render empty")
	}
	// A changed buffer, still within the cooldown, must return the CACHED render.
	cached := m.renderStreaming("# Title\n\nfirst body, now much longer with more text", 80)
	if cached != first {
		t.Fatal("expected cached reuse within cooldown, got a fresh render")
	}
	// Force the cooldown to have elapsed : the next call recomputes for the new
	// text, so the output changes.
	m.streamRenderAt = m.streamRenderAt.Add(-2 * time.Second)
	fresh := m.renderStreaming("# Title\n\nfirst body, now much longer with more text", 80)
	if fresh == first {
		t.Fatal("expected a fresh render after the cooldown elapsed")
	}
}

// A width change (resize) must invalidate the cache immediately — otherwise the
// reply would render at the old width until the cooldown expired.
func TestRenderStreaming_WidthChangeInvalidatesCache(t *testing.T) {
	m := &Messages{theme: theme.Default(), width: 80}
	first := m.renderStreaming("# Title\n\nbody text here", 80)
	m.width = 50
	resized := m.renderStreaming("# Title\n\nbody text here", 50)
	if resized == first {
		t.Fatal("expected re-render at the new width, got the old-width cache")
	}
}
