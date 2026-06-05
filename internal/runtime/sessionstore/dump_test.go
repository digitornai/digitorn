package sessionstore

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDump_DiskLayoutForDocs(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-DEMO-abc123"

	flusher, _ := NewDiskFlusher(DiskFlusherConfig{
		Paths: paths, NumShards: 4, QueueCapPerShard: 4096, BatchMax: 100, FlushInterval: 2,
	})
	flusher.Start()
	defer func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		flusher.Stop(ctx)
	}()
	bus, _ := NewBus(BusConfig{Paths: paths, Flusher: flusher})
	bus.Start(context.Background())
	defer bus.Stop(context.Background())

	events := []Event{
		{Type: EventSessionStarted, SessionID: sid, Meta: &MetaPayload{Title: "Demo Chat", Workspace: "/workspace", Workdir: "/workspace/proj"}},
		{Type: EventUserMessage, SessionID: sid, Message: &MessagePayload{Role: "user", Content: "List the files."}},
		{Type: EventToolCall, SessionID: sid, Tool: &ToolPayload{CallID: "c1", Name: "filesystem.ls", Arguments: map[string]any{"path": "/workspace/proj"}}},
		{Type: EventToolResult, SessionID: sid, Tool: &ToolPayload{CallID: "c1", Status: "completed", Output: []string{"main.go", "go.mod"}, DurationMs: 4}},
		{Type: EventAssistantMessage, SessionID: sid, Message: &MessagePayload{Role: "assistant", Content: "I see 2 files: main.go and go.mod."}},
		{Type: EventCostUpdate, SessionID: sid, Cost: &CostPayload{TokensIn: 142, TokensOut: 38, UsdTotal: 0.0019}},
	}
	for _, ev := range events {
		if _, err := bus.Append(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if err := flusher.Flush(ctx2); err != nil {
		t.Fatal(err)
	}

	state, _ := bus.State(sid)
	c := bus.Compactor(CompactorConfig{})
	if _, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync, Gate: bus}); err != nil {
		t.Fatal(err)
	}
	if err := flusher.Flush(ctx2); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	out.WriteString("\n==================== DISK LAYOUT FOR ONE SESSION ====================\n")
	out.WriteString("root = $TMP\n\n")
	filepath.WalkDir(tmp, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(tmp, path)
		if rel == "." {
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator))
		indent := strings.Repeat("  ", depth)
		if d.IsDir() {
			fmt.Fprintf(&out, "%s%s/\n", indent, d.Name())
		} else {
			info, _ := d.Info()
			fmt.Fprintf(&out, "%s%s   (%d bytes)\n", indent, d.Name(), info.Size())
		}
		return nil
	})

	dir := paths.SessionDir(sid)

	out.WriteString("\n--- events.jsonl (first 3 lines, truncated to 120 chars) ---\n")
	if data, err := os.ReadFile(filepath.Join(dir, "events.jsonl")); err == nil {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		for i, line := range lines {
			if i >= 3 {
				fmt.Fprintf(&out, "  ... %d more lines\n", len(lines)-3)
				break
			}
			truncLine := line
			if len(truncLine) > 120 {
				truncLine = truncLine[:120] + "..."
			}
			fmt.Fprintf(&out, "  %s\n", truncLine)
		}
	}

	out.WriteString("\n--- meta.json ---\n")
	if data, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil {
		fmt.Fprintf(&out, "  %s\n", string(data))
	}

	out.WriteString("\n--- snapshot.json (first 400 chars) ---\n")
	if data, err := os.ReadFile(filepath.Join(dir, "snapshot.json")); err == nil {
		s := string(data)
		if len(s) > 400 {
			s = s[:400] + "..."
		}
		fmt.Fprintf(&out, "  %s\n", s)
	}

	out.WriteString("====================================================================\n")
	t.Log(out.String())
}
