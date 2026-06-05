package appmgr_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// repoChatSimpleDir locates bin/test-apps/chat-simple at the repo root.
// The bundle is the fixture used by the R-5 live test (PowerShell
// script bin/live-test-chat.ps1) — this Go test runs the install +
// snapshot path in-process so the live test starts from a known-good
// baseline.
func repoChatSimpleDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "bin", "test-apps", "chat-simple", "app.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return filepath.Dir(candidate)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("bin/test-apps/chat-simple not found above CWD")
	return ""
}

// TestInstall_LiveChatBundle_Compiles_AndEmbedsAnthropicKey is the
// Go-side prerequisite of R-5 : the chat-simple bundle compiles, the
// env-var passthrough resolves the real Anthropic key into the .dgc
// bytecode, and the RuntimeApp ressuscitated from the snapshot carries
// it on the brain.config.api_key field. R-5 then flips BYOK via PUT
// /api/apps/chat-simple/byok and the engine forwards the key directly
// to the provider, bypassing the gateway.
func TestInstall_LiveChatBundle_Compiles_AndEmbedsAnthropicKey(t *testing.T) {
	const fakeKey = "sk-openai-livetest-resolved-by-go-test"
	t.Setenv("OPENAI_API_KEY", fakeKey)

	m, _, _ := newTestManager(t)
	src := repoChatSimpleDir(t)

	ctx := context.Background()
	meta, err := m.Install(ctx, src, "")
	if err != nil {
		t.Fatalf("install chat-simple : %v", err)
	}
	if meta.AppID != "chat-simple" {
		t.Errorf("app_id = %q, want chat-simple", meta.AppID)
	}
	if meta.Version != "0.1.0" {
		t.Errorf("version = %q, want 0.1.0", meta.Version)
	}
	// Default BYOK : false. Live test will PUT /byok {enabled: true}.
	if meta.BYOK {
		t.Error("BYOK should default to false at install ; operator flips it explicitly")
	}

	ra, err := m.Get(ctx, "chat-simple")
	if err != nil {
		t.Fatalf("get chat-simple : %v", err)
	}
	if len(ra.Definition.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(ra.Definition.Agents))
	}
	agent := ra.Definition.Agents[0]
	if agent.ID != "assistant" {
		t.Errorf("agent id = %q, want assistant", agent.ID)
	}
	if agent.Brain.Provider != "openai" {
		t.Errorf("provider = %q, want openai", agent.Brain.Provider)
	}
	if agent.Brain.Model != "gpt-5-mini" {
		t.Errorf("model = %q", agent.Brain.Model)
	}

	// Compiler resolved the env-var into a real literal in the brain.
	gotKey, ok := agent.Brain.Config["api_key"].(string)
	if !ok {
		t.Fatalf("brain.config.api_key is not a string : %T", agent.Brain.Config["api_key"])
	}
	if gotKey != fakeKey {
		t.Errorf("api_key not resolved by compiler : got %q, want %q", gotKey, fakeKey)
	}
}

// TestInstall_LiveChatBundle_MissingEnvKeyPassesThrough proves that
// when ANTHROPIC_API_KEY is NOT set, the compiler doesn't fail (we use
// lenient mode by default) ; the placeholder stays embedded. Means an
// operator can install the bundle without the secret being available
// at install time and provision it later.
func TestInstall_LiveChatBundle_MissingEnvKeyPassesThrough(t *testing.T) {
	os.Unsetenv("OPENAI_API_KEY")

	m, _, _ := newTestManager(t)
	src := repoChatSimpleDir(t)

	if _, err := m.Install(context.Background(), src, ""); err != nil {
		t.Fatalf("install with missing env should still succeed (lenient mode) : %v", err)
	}
	ra, err := m.Get(context.Background(), "chat-simple")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := ra.Definition.Agents[0].Brain.Config["api_key"].(string)
	if got != "{{env.OPENAI_API_KEY}}" {
		t.Errorf("placeholder not preserved : got %q", got)
	}
}
