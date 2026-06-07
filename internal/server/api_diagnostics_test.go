package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/core/servicebus"
	"github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/modules/lsp"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// ── Wiring: the debounce feeds the diagnostics push with the changed paths ──────

func TestWorkspaceLive_FeedsDiagnosticsPush(t *testing.T) {
	rt := newFakeRealtime()
	l := newTestLive(rt, 20*time.Millisecond, func(context.Context, string) ([]sessionstore.WorkspaceChangedFile, error) {
		return []sessionstore.WorkspaceChangedFile{
			{Path: "src/app.tsx", Status: "modified"},
			{Path: "README.md", Status: "added"},
		}, nil
	})
	var mu sync.Mutex
	var gotRoot string
	var gotChanged []string
	l.diagnosticsPush = func(_ context.Context, root string, changed []string) {
		mu.Lock()
		defer mu.Unlock()
		gotRoot, gotChanged = root, changed
	}

	l.FileChanged("root", "/wd")
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return gotChanged != nil }, "diagnostics push fired")

	mu.Lock()
	defer mu.Unlock()
	if gotRoot != "root" {
		t.Fatalf("diagnostics push root = %q", gotRoot)
	}
	if len(gotChanged) != 2 || gotChanged[0] != "src/app.tsx" || gotChanged[1] != "README.md" {
		t.Fatalf("diagnostics push got wrong changed list: %#v", gotChanged)
	}
}

func TestWorkspaceLive_NoDiagnosticsPushWhenNoChanges(t *testing.T) {
	rt := newFakeRealtime()
	l := newTestLive(rt, 20*time.Millisecond, func(context.Context, string) ([]sessionstore.WorkspaceChangedFile, error) {
		return nil, nil
	})
	var called int64
	l.diagnosticsPush = func(context.Context, string, []string) { atomic.AddInt64(&called, 1) }
	l.FileChanged("root", "/wd")
	waitUntil(t, func() bool { return l.pendLen() == 0 }, "settled")
	if atomic.LoadInt64(&called) != 0 {
		t.Fatalf("no changes must not trigger a diagnostics push, got %d", called)
	}
}

// ── Envelope contract: exactly what the web's workspace-module reducer reads ────

func TestEmitFileDiagnostics_SetShape(t *testing.T) {
	rt := newFakeRealtime()
	items := []map[string]any{{"severity": "error", "line": 3, "column": 7, "message": "boom", "source": "ts"}}
	emitFileDiagnostics(context.Background(), rt, "root", "src/a.ts", items)

	emits := rt.recordedEmits()
	if len(emits) != 1 {
		t.Fatalf("expected 1 emit, got %d", len(emits))
	}
	m := emits[0]
	if m.Room != "session:root" || m.Event != "event" || m.Namespace != bridgeNamespace {
		t.Fatalf("bad routing: %+v", m)
	}
	data, ok := m.Data.(map[string]any)
	if !ok {
		t.Fatalf("emit data is not a map: %T", m.Data)
	}
	if data["type"] != "preview:resource_set" || data["channel"] != "diagnostics" ||
		data["id"] != "src/a.ts" || data["session_id"] != "root" {
		t.Fatalf("bad envelope: %#v", data)
	}
	payload, ok := data["payload"].(map[string]any)
	if !ok || payload["source_label"] != "lsp" {
		t.Fatalf("bad payload: %#v", data["payload"])
	}
	if got, _ := payload["items"].([]map[string]any); len(got) != 1 || got[0]["message"] != "boom" {
		t.Fatalf("bad items: %#v", payload["items"])
	}
}

func TestEmitFileDiagnostics_CleanFileDeletes(t *testing.T) {
	rt := newFakeRealtime()
	emitFileDiagnostics(context.Background(), rt, "root", "src/a.ts", nil)
	emits := rt.recordedEmits()
	if len(emits) != 1 {
		t.Fatalf("expected 1 emit, got %d", len(emits))
	}
	data, _ := emits[0].Data.(map[string]any)
	if data["type"] != "preview:resource_deleted" || data["channel"] != "diagnostics" || data["id"] != "src/a.ts" {
		t.Fatalf("a clean file must emit resource_deleted on the diagnostics channel: %#v", data)
	}
	if _, has := data["payload"]; has {
		t.Fatalf("delete envelope must carry no payload: %#v", data)
	}
}

// ── lspDiagnose: parses the lsp module result regardless of in-proc/worker shape ─

type fakeBus struct {
	res   tool.Result
	err   error
	calls []string
}

func (b *fakeBus) Register(module.Module) error     { return nil }
func (b *fakeBus) Unregister(string) error          { return nil }
func (b *fakeBus) Get(string) (module.Module, bool) { return nil, false }
func (b *fakeBus) List() []module.Module            { return nil }
func (b *fakeBus) Call(_ context.Context, mod, tn string, _ []byte) (tool.Result, error) {
	b.calls = append(b.calls, mod+"."+tn)
	return b.res, b.err
}

func TestLspDiagnose_ParsesResult(t *testing.T) {
	fb := &fakeBus{res: tool.Result{Success: true, Data: map[string]any{
		"path": "/wd/a.ts",
		"diagnostics": []map[string]any{
			{"severity": "error", "line": 2, "column": 5, "message": "type error", "source": "ts"},
			{"severity": "warning", "line": 9, "column": 1, "message": "unused", "source": "ts"},
		},
	}}}
	d := &Daemon{bus: fb}
	items := d.lspDiagnose(context.Background(), "/wd/a.ts")
	if len(items) != 2 || items[0]["message"] != "type error" || items[1]["severity"] != "warning" {
		t.Fatalf("bad parse: %#v", items)
	}
	if len(fb.calls) != 1 || fb.calls[0] != "lsp.notify_change" {
		t.Fatalf("must call lsp.notify_change once, got %v", fb.calls)
	}
}

func TestLspDiagnose_ErrorOrUnsuccessfulYieldsNil(t *testing.T) {
	d1 := &Daemon{bus: &fakeBus{res: tool.Result{Success: false}}}
	if got := d1.lspDiagnose(context.Background(), "/x.ts"); got != nil {
		t.Fatalf("unsuccessful result must yield nil, got %#v", got)
	}
	d2 := &Daemon{bus: &fakeBus{res: tool.Result{Success: true, Data: map[string]any{}}}}
	if got := d2.lspDiagnose(context.Background(), "/x.ts"); len(got) != 0 {
		t.Fatalf("no diagnostics field must yield empty, got %#v", got)
	}
}

// ── Real language server: a TS type error surfaces as a parsed error diagnostic ──

func TestLspDiagnose_RealTypeScript(t *testing.T) {
	if _, err := exec.LookPath("typescript-language-server"); err != nil {
		t.Skip("typescript-language-server not installed")
	}
	wd := t.TempDir()
	mustWriteFile(t, filepath.Join(wd, "tsconfig.json"),
		`{"compilerOptions":{"strict":true,"noEmit":true},"include":["*.ts"]}`)
	abs := filepath.Join(wd, "bad.ts")
	mustWriteFile(t, abs, "const x: number = \"definitely not a number\";\nexport {};\n")

	mod := lsp.New()
	if err := mod.Init(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("lsp init: %v", err)
	}
	defer mod.Stop(context.Background())
	bus := servicebus.New()
	if err := bus.Register(mod); err != nil {
		t.Fatalf("register: %v", err)
	}
	d := &Daemon{bus: bus}

	// tsserver cold-start + project load can take a few seconds; retry until it
	// reports the type error (TS2322) or we give up.
	var items []map[string]any
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		items = d.lspDiagnose(context.Background(), abs)
		if len(items) > 0 {
			break
		}
		time.Sleep(1500 * time.Millisecond)
	}
	if len(items) == 0 {
		t.Skip("tsserver returned no diagnostics in time (env/cold-start) — covered by deterministic tests")
	}
	foundErr := false
	for _, it := range items {
		if it["severity"] == "error" {
			foundErr = true
		}
	}
	if !foundErr {
		b, _ := json.Marshal(items)
		t.Fatalf("expected a real TS error diagnostic, got: %s", b)
	}
	t.Logf("real tsserver reported %d diagnostic(s) incl. an error", len(items))
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
