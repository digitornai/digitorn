package processor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/background/adapter"
	"github.com/digitornai/digitorn/internal/background/adapter/webhook"
	"github.com/digitornai/digitorn/internal/background/channels"
	"github.com/digitornai/digitorn/internal/background/daemonclient"
)

// TestManager_WebhookIngress proves the real inbound ingress: a webhook POST is
// routed by the manager's intake to the right armed trigger, durably enqueued,
// then claimed + processed into a daemon invocation.
func TestManager_WebhookIngress(t *testing.T) {
	st := newStore(t)
	fd := &fakeDaemon{}
	client := daemonclient.New(fd.server(t).URL, "")

	wh := webhook.New([]webhook.Provider{{Name: "wh", Path: "/hook/x", Auth: "none"}})
	reg := adapter.NewRegistry()
	reg.Register(wh)
	mgr := NewManager(st, reg)

	if _, err := mgr.Arm(context.Background(), TriggerSpec{
		AppID: "demo", Provider: "wh", Adapter: "webhook", DefaultAgent: "main",
		Activation: channels.ActivationConfig{Message: "from {{event.payload.who}}", Session: channels.SessionPerEvent},
	}); err != nil {
		t.Fatalf("arm: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = mgr.Start(ctx) }() // wires the routing sink into the webhook adapter

	h := mgr.Handler()
	if h == nil {
		t.Fatal("manager should expose an HTTP handler (webhook registered)")
	}

	// POST until the adapter's sink is wired (Start is async).
	var code int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/hook/x", strings.NewReader(`{"who":"alice"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Delivery-Id", "d-1")
		h.ServeHTTP(rr, req)
		code = rr.Code
		if code == http.StatusAccepted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if code != http.StatusAccepted {
		t.Fatalf("webhook ingress code = %d", code)
	}

	// The event was durably enqueued via the routing intake.
	jobs, err := st.Claim(context.Background(), 1, 30_000_000_000)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v n=%d", err, len(jobs))
	}
	if jobs[0].TriggerID == "" || jobs[0].AppID != "demo" {
		t.Fatalf("job not routed to trigger: %+v", jobs[0])
	}

	// Process → daemon invocation with the rendered message.
	p := New(st, client, reg, nil, discard())
	if err := p.Process(context.Background(), jobs[0]); err != nil {
		t.Fatalf("process: %v", err)
	}
	if fd.creates != 1 {
		t.Fatalf("expected 1 daemon create, got %d", fd.creates)
	}
}

// TestManager_UnknownProviderRejected proves the routing intake refuses an event
// with no armed trigger (so it isn't silently swallowed).
func TestManager_UnknownProviderRejected(t *testing.T) {
	st := newStore(t)
	mgr := NewManager(st, adapter.NewRegistry())
	err := mgr.Sink()(context.Background(), adapter.Event{Provider: "ghost", DedupKey: "x"})
	if err == nil {
		t.Fatal("event for an unarmed provider must be rejected")
	}
}
