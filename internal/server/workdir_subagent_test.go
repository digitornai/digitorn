package server

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// TestSessionPathPolicies_SubAgentSharesRootWorkdir : a delegated sub-agent runs
// in an isolated sub-session (root::agent::<runID>) but must SHARE the root
// session's workdir, so the coordinator and its sub-agents read/write the same
// files. The sub-session carries no workdir of its own, so PathPolicyFor must
// resolve it from the root. This is the fix for the "coordinator wrote files the
// sub-agent couldn't see" mismatch.
func TestSessionPathPolicies_SubAgentSharesRootWorkdir(t *testing.T) {
	_, bus, _, _, cleanup := setupBridge(t)
	defer cleanup()

	const root = "root1"
	wd := t.TempDir()
	if _, err := bus.AppendDurable(context.Background(), sessionstore.Event{
		Type: sessionstore.EventSessionStarted, SessionID: root, AppID: "app", UserID: "u",
		Meta: &sessionstore.MetaPayload{Workdir: wd},
	}); err != nil {
		t.Fatalf("seed root workdir: %v", err)
	}

	src := sessionPathPolicies{store: bus} // apps nil : no extra constraints needed

	rootPol, ok := src.PathPolicyFor("app", root)
	if !ok {
		t.Fatal("root session must resolve a workdir policy")
	}

	subPol, ok := src.PathPolicyFor("app", root+"::agent::run-7")
	if !ok {
		t.Fatal("sub-agent must inherit the root session's workdir policy")
	}
	if subPol.Root() != rootPol.Root() {
		t.Errorf("sub-agent workdir = %q, want the root's %q (shared workspace)", subPol.Root(), rootPol.Root())
	}

	// Nested sub-agent resolves to the SAME top-level root, not the intermediate.
	nestedPol, ok := src.PathPolicyFor("app", root+"::agent::run-7::agent::run-9")
	if !ok || nestedPol.Root() != rootPol.Root() {
		t.Errorf("nested sub-agent must share the top-level root workdir: got %q ok=%v", nestedPol.Root(), ok)
	}

	// A genuinely unrelated session with no workdir must get no policy — the
	// sub-agent resolution must never leak a workdir to an unrelated session.
	if _, ok := src.PathPolicyFor("app", "unrelated-session"); ok {
		t.Error("an unrelated session with no workdir must get no policy")
	}
}
