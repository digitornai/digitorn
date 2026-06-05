package client

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLive_StreamReceivesFinal connects exactly like the TUI does
// (REST session + Socket.IO join), posts a message, and records every
// envelope received. It asserts the final assistant_message arrives
// with content — i.e. the streaming flood doesn't starve the final.
//
// Opt-in : set DIGITORN_LIVE=1 and DIGITORN_URL ; the cached
// credentials.json supplies the token + user id. Skipped otherwise.
func TestLive_StreamReceivesFinal(t *testing.T) {
	if os.Getenv("DIGITORN_LIVE") != "1" {
		t.Skip("set DIGITORN_LIVE=1 to run the live streaming test")
	}
	base := os.Getenv("DIGITORN_URL")
	if base == "" {
		base = "http://127.0.0.1:8000"
	}
	creds, err := LoadCredentials()
	if err != nil || creds == nil {
		t.Fatalf("load credentials: %v", err)
	}
	appID := os.Getenv("DIGITORN_APP")
	if appID == "" {
		appID = "approve-probe"
	}

	c, err := New(Options{BaseURL: base, BearerToken: creds.AccessToken, UserID: DefaultUserID(creds)})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := context.Background()
	sess, err := c.CreateSession(ctx, appID, CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	rt, err := NewRealtime(RealtimeOptions{BaseURL: base, Token: creds.AccessToken, UserID: DefaultUserID(creds)})
	if err != nil {
		t.Fatalf("realtime: %v", err)
	}
	var mu sync.Mutex
	counts := map[string]int{}
	var finalContent string
	rt.OnEnvelope(func(env Envelope) {
		mu.Lock()
		counts[env.Type]++
		if env.Type == "assistant_message" {
			if v, ok := env.Payload["content"].(string); ok {
				finalContent = v
			}
		}
		mu.Unlock()
	})
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := rt.Connect(cctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer rt.Close()
	if err := rt.JoinSession(appID, sess.SessionID); err != nil {
		t.Fatalf("join: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if _, err := c.PostMessage(ctx, appID, sess.SessionID, "Write a short paragraph about the sea. No tools.", ""); err != nil {
		t.Fatalf("post: %v", err)
	}

	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := counts["turn_ended"] > 0
		mu.Unlock()
		if done {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()
	t.Logf("received envelope types: %v", counts)
	t.Logf("final assistant content: %q", finalContent)
	if counts["assistant_delta"] == 0 {
		t.Errorf("no assistant_delta received (streaming not flowing to client)")
	}
	if counts["assistant_message"] == 0 || strings.TrimSpace(finalContent) == "" {
		t.Errorf("final assistant_message missing or empty — this is the 'no response' bug")
	}
}
