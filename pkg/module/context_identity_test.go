package module

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

func TestIDGettersFallBackToToolIdentity(t *testing.T) {
	ctx := tool.WithIdentity(context.Background(), tool.Identity{
		AppID: "app-1", UserID: "user-1", SessionID: "sess-1", AgentID: "agent-1",
	})
	if got := AppID(ctx); got != "app-1" {
		t.Errorf("AppID = %q, want app-1", got)
	}
	if got := UserID(ctx); got != "user-1" {
		t.Errorf("UserID = %q, want user-1", got)
	}
	if got := SessionID(ctx); got != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", got)
	}
	if got := AgentID(ctx); got != "agent-1" {
		t.Errorf("AgentID = %q, want agent-1", got)
	}
}

func TestExplicitKeysWinOverIdentity(t *testing.T) {
	ctx := tool.WithIdentity(context.Background(), tool.Identity{AppID: "from-identity"})
	ctx = WithAppID(ctx, "explicit")
	if got := AppID(ctx); got != "explicit" {
		t.Errorf("AppID = %q, want explicit", got)
	}
}
