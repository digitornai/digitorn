package pieces

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

func liveBridgeOrSkip(t *testing.T) *Bridge {
	t.Helper()
	b := &Bridge{triggerPort: 9234}
	if _, err := b.GetPieceAuth("vercel"); err != nil {
		t.Skipf("live pieces bridge not reachable on :9234 (%v)", err)
	}
	return b
}

func TestPieceRequiresAuth_LiveBridge(t *testing.T) {
	m := &Module{bridge: liveBridgeOrSkip(t)}
	cases := map[string]bool{
		"vercel":            true,
		"flow_helper":       false,
		"no_such_piece_xyz": false,
	}
	for piece, want := range cases {
		if got := m.pieceRequiresAuth(piece); got != want {
			t.Errorf("pieceRequiresAuth(%q) = %v, want %v", piece, got, want)
		}
	}
}

func TestInvoke_NotConnected_FailsClean(t *testing.T) {
	m := &Module{bridge: liveBridgeOrSkip(t)}
	ctx := tool.WithIdentity(context.Background(), tool.Identity{UserID: "user-with-no-vercel"})

	res, err := m.Invoke(ctx, "ap_vercel__create_deployment", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke returned transport error: %v", err)
	}
	if res.Success {
		t.Fatalf("expected failure for unconnected connector, got success")
	}
	if !strings.Contains(strings.ToLower(res.Error), "not connected") {
		t.Fatalf("expected actionable 'not connected' error, got: %q", res.Error)
	}
}

func TestAuthFailureClassifiers(t *testing.T) {
	term := func(msg string) tool.Result { return tool.Result{Success: false, Error: msg} }

	terminalCases := []string{
		`{"ok":false,"error":"An API error occurred: not_authed"}`,
		"invalid_auth",
		"token_revoked",
		"account_inactive",
		"oauth error: invalid_grant",
	}
	for _, c := range terminalCases {
		if !isTerminalAuthFailure(term(c)) {
			t.Errorf("isTerminalAuthFailure(%q) = false, want true", c)
		}
		if !isAuthFailure(term(c)) {
			t.Errorf("isAuthFailure(%q) = false, want true (terminal implies auth failure)", c)
		}
	}

	transientAuth := []string{"HTTP 401 unauthorized", "token has expired"}
	for _, c := range transientAuth {
		if !isAuthFailure(term(c)) {
			t.Errorf("isAuthFailure(%q) = false, want true", c)
		}
		if isTerminalAuthFailure(term(c)) {
			t.Errorf("isTerminalAuthFailure(%q) = true, want false (should not force reconnect)", c)
		}
	}

	nonAuth := []string{"rate limited", "not found", "500 internal error", ""}
	for _, c := range nonAuth {
		if isAuthFailure(term(c)) || isTerminalAuthFailure(term(c)) {
			t.Errorf("classifiers flagged non-auth error %q", c)
		}
	}
	if isTerminalAuthFailure(tool.Result{Success: true, Error: "not_authed"}) {
		t.Errorf("success result must never be an auth failure")
	}
}
