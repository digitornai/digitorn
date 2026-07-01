package processor

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/digitornai/digitorn/internal/background/adapter"
	"github.com/digitornai/digitorn/internal/background/channels"
	"github.com/digitornai/digitorn/internal/background/daemonclient"
)

// FuzzToolBody asserts the channel-facing approval renderer never panics and always
// returns valid, bounded UTF-8 — whatever a (possibly hostile or broken) model puts
// in the tool params: control bytes, lone surrogates, gigabytes of newlines, weird
// paths. This is the string that gets posted to Discord/Telegram, so a panic here
// would crash the approval pump.
func FuzzToolBody(f *testing.F) {
	f.Add("filesystem.write", "C:\\a\\b\\bot.py", "print(1)\n", "", "medium")
	f.Add("bash.run", "", "", "rm -rf / && echo done", "high")
	f.Add("", "", "", "", "")
	f.Add("x", "/etc/\x00passwd", "\x00\x01\x02\xff", "", "low")
	f.Add("t", "日本語/ファイル.go", strings.Repeat("ligne\n", 5000), "", "")

	f.Fuzz(func(t *testing.T, tool, path, content, command, risk string) {
		ap := daemonclient.Approval{
			Kind:      "tool_call",
			ToolName:  tool,
			RiskLevel: risk,
			ToolParams: map[string]any{
				"path":    path,
				"content": content,
				"command": command,
				"extra":   123,
				"flag":    true,
			},
		}
		got := toolBody(ap) // must not panic
		if !utf8.ValidString(got) {
			t.Fatalf("toolBody produced invalid UTF-8 for tool=%q path=%q", tool, path)
		}
		if len(got) > 4000 {
			t.Fatalf("toolBody must stay bounded, got %d bytes", len(got))
		}
	})
}

func TestToolBody_EdgeCases(t *testing.T) {
	cases := []struct {
		name   string
		ap     daemonclient.Approval
		expect func(t *testing.T, out string)
	}{
		{
			name: "unicode path → basename only",
			ap: daemonclient.Approval{ToolName: "filesystem.write",
				ToolParams: map[string]any{"path": "/home/utilisateur/projets/日本語/ファイル.go", "content": "package main"}},
			expect: func(t *testing.T, out string) {
				if !strings.Contains(out, "ファイル.go") {
					t.Errorf("want unicode basename, got %q", out)
				}
				if strings.Contains(out, "/home/utilisateur") {
					t.Errorf("must not leak the full path: %q", out)
				}
			},
		},
		{
			name: "windows path basename",
			ap: daemonclient.Approval{ToolName: "filesystem.write",
				ToolParams: map[string]any{"path": `C:\Users\ASUS\workdir\bot.py`, "content": "x"}},
			expect: func(t *testing.T, out string) {
				if !strings.Contains(out, "bot.py") || strings.Contains(out, `C:\Users`) {
					t.Errorf("windows basename failed: %q", out)
				}
			},
		},
		{
			name: "no params at all → human fallback, never empty",
			ap:   daemonclient.Approval{ToolName: "", ToolParams: nil},
			expect: func(t *testing.T, out string) {
				if strings.TrimSpace(out) == "" {
					t.Error("toolBody must never be empty")
				}
			},
		},
		{
			name: "control chars in content do not break the block",
			ap: daemonclient.Approval{ToolName: "filesystem.write",
				ToolParams: map[string]any{"path": "a.txt", "content": "a\x00b\x07c\x1bd"}},
			expect: func(t *testing.T, out string) {
				if !utf8.ValidString(out) {
					t.Errorf("non-utf8 output: %q", out)
				}
			},
		},
		{
			name: "scalar params rendered as sorted lines",
			ap: daemonclient.Approval{ToolName: "http.get",
				ToolParams: map[string]any{"url": "https://x", "method": "GET", "timeout": 30}},
			expect: func(t *testing.T, out string) {
				if !strings.Contains(out, "http.get") {
					t.Errorf("missing tool name: %q", out)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := toolBody(c.ap)
			if !utf8.ValidString(out) {
				t.Fatalf("invalid UTF-8: %q", out)
			}
			c.expect(t, out)
		})
	}
}

// TestDeliverReply_MissingAdapterGraceful: a push destination that names an adapter
// not in the registry must be a logged no-op, never a panic or a crash.
func TestDeliverReply_MissingAdapterGraceful(t *testing.T) {
	reg := adapter.NewRegistry() // empty — no adapter registered
	p := New(nil, nil, reg, nil, discard())
	// Must not panic even though "ghost" is unknown.
	p.deliverReply(context.Background(),
		adapter.Event{Adapter: "ghost", ReplyRef: map[string]any{"to": "x"}},
		channels.Activation{Deliver: &channels.Destination{Adapter: "ghost", Ref: map[string]any{"chan": "1"}}},
		TriggerSpec{},
		"hello")
	// No assertion beyond "did not panic / did not send" — reaching here is the pass.
}

// TestDeliverReply_EmptyTextNoSend: an empty/whitespace reply is never delivered
// (the channel should not see a blank message).
func TestDeliverReply_EmptyTextNoSend(t *testing.T) {
	ad := &prompterAdapter{}
	reg := adapter.NewRegistry()
	reg.Register(ad)
	p := New(nil, nil, reg, nil, discard())
	p.deliverReply(context.Background(),
		adapter.Event{Adapter: "fake", ReplyRef: map[string]any{"to": "x"}},
		channels.Activation{}, TriggerSpec{}, "   \n  ")
	if sent, _, _ := ad.snapshot(); len(sent) != 0 {
		t.Fatalf("whitespace-only reply must not be sent, got %v", sent)
	}
}
