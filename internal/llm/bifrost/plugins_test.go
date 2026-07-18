package bifrost

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	schemas "github.com/maximhq/bifrost/core/schemas"
)

func newCtxForTest() *schemas.BifrostContext {
	bc, _ := schemas.NewBifrostContextWithTimeout(context.TODO(), 5*time.Second)
	return bc
}

func chatReq(provider string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.ModelProvider(provider),
			Model:    "test-model",
		},
	}
}

// ---------- AuditPlugin ----------

func TestAuditPlugin_LogsLineOnSuccess(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	p := NewAuditPlugin(logger, true)

	ctx := newCtxForTest()
	req, _, err := p.PreLLMHook(ctx, chatReq("anthropic"))
	if err != nil {
		t.Fatal(err)
	}
	if req == nil {
		t.Fatal("request mutated to nil")
	}

	resp := &schemas.BifrostResponse{}
	_, _, err = p.PostLLMHook(ctx, resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "llm.call") {
		t.Fatalf("expected audit log line; got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "success=true") {
		t.Fatalf("expected success=true ; got: %s", buf.String())
	}
}

func TestAuditPlugin_DisabledSuppresses(t *testing.T) {
	var buf bytes.Buffer
	p := NewAuditPlugin(slog.New(slog.NewTextHandler(&buf, nil)), false)
	ctx := newCtxForTest()
	p.PreLLMHook(ctx, chatReq("anthropic"))
	p.PostLLMHook(ctx, &schemas.BifrostResponse{}, nil)
	if buf.Len() != 0 {
		t.Fatalf("disabled audit must not log ; got: %s", buf.String())
	}
}

func TestAuditPlugin_FailureLogged(t *testing.T) {
	var buf bytes.Buffer
	p := NewAuditPlugin(slog.New(slog.NewTextHandler(&buf, nil)), true)
	ctx := newCtxForTest()
	p.PreLLMHook(ctx, chatReq("openai"))
	msg := "bad key"
	berr := &schemas.BifrostError{Error: &schemas.ErrorField{Message: msg}}
	p.PostLLMHook(ctx, nil, berr)
	if !strings.Contains(buf.String(), "success=false") {
		t.Fatalf("expected success=false; got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), msg) {
		t.Fatalf("expected error message in log; got: %s", buf.String())
	}
}

// ---------- MetricsPlugin ----------

func TestMetricsPlugin_CountsAndLatency(t *testing.T) {
	m := NewMetricsPlugin()

	for i := 0; i < 5; i++ {
		ctx := newCtxForTest()
		m.PreLLMHook(ctx, chatReq("anthropic"))
		time.Sleep(2 * time.Millisecond)
		m.PostLLMHook(ctx, &schemas.BifrostResponse{}, nil)
	}
	for i := 0; i < 3; i++ {
		ctx := newCtxForTest()
		m.PreLLMHook(ctx, chatReq("openai"))
		time.Sleep(1 * time.Millisecond)
		typ := "rate_limited"
		m.PostLLMHook(ctx, nil, &schemas.BifrostError{Type: &typ, Error: &schemas.ErrorField{Message: "x"}})
	}

	s := m.Stats()
	if s.TotalRequests != 8 {
		t.Fatalf("total_requests: %d", s.TotalRequests)
	}
	if s.TotalErrors != 3 {
		t.Fatalf("total_errors: %d", s.TotalErrors)
	}
	if s.TotalAvgLatencyMs <= 0 {
		t.Fatalf("avg latency missing: %f", s.TotalAvgLatencyMs)
	}
	ant := s.PerProvider["anthropic"]
	if ant.Requests != 5 || ant.Errors != 0 {
		t.Fatalf("anthropic: %+v", ant)
	}
	oai := s.PerProvider["openai"]
	if oai.Requests != 3 || oai.Errors != 3 {
		t.Fatalf("openai: %+v", oai)
	}
	if oai.MaxLatencyMs <= 0 {
		t.Fatalf("max latency missing: %f", oai.MaxLatencyMs)
	}
}

// ---------- CircuitBreakerPlugin ----------

func TestCircuitBreaker_OpensAfterThresholdFailures(t *testing.T) {
	cb := NewCircuitBreakerPlugin(3, 1*time.Second, 500*time.Millisecond)

	for i := 0; i < 3; i++ {
		ctx := newCtxForTest()
		req, sc, _ := cb.PreLLMHook(ctx, chatReq("flaky-provider"))
		if sc != nil {
			t.Fatalf("not expected to short-circuit yet (i=%d)", i)
		}
		_ = req
		cb.PostLLMHook(ctx, nil, &schemas.BifrostError{Error: &schemas.ErrorField{Message: "boom"}})
	}

	// 4th call should be short-circuited.
	ctx := newCtxForTest()
	_, sc, _ := cb.PreLLMHook(ctx, chatReq("flaky-provider"))
	if sc == nil {
		t.Fatal("circuit must be open after 3 failures within window")
	}
	if sc.Error == nil || sc.Error.Type == nil || *sc.Error.Type != "circuit_breaker_open" {
		t.Fatalf("expected typed circuit_breaker_open error; got: %+v", sc.Error)
	}

	st := cb.Stats()
	if len(st.OpenProviders) != 1 || st.OpenProviders[0] != "flaky-provider" {
		t.Fatalf("open providers: %v", st.OpenProviders)
	}
	if st.TotalShorts == 0 {
		t.Fatal("totalshorts not incremented")
	}
}

func TestCircuitBreaker_ClosesAfterCooldown(t *testing.T) {
	cb := NewCircuitBreakerPlugin(2, 1*time.Second, 100*time.Millisecond)

	for i := 0; i < 2; i++ {
		ctx := newCtxForTest()
		cb.PreLLMHook(ctx, chatReq("p"))
		cb.PostLLMHook(ctx, nil, &schemas.BifrostError{Error: &schemas.ErrorField{Message: "x"}})
	}
	// Confirm open.
	if _, sc, _ := cb.PreLLMHook(newCtxForTest(), chatReq("p")); sc == nil {
		t.Fatal("should be open")
	}
	// Wait for cooldown.
	time.Sleep(150 * time.Millisecond)
	if _, sc, _ := cb.PreLLMHook(newCtxForTest(), chatReq("p")); sc != nil {
		t.Fatalf("should be closed after cooldown ; got %+v", sc)
	}
}

// A quota block (429) is the user's own limit, NOT a provider-health
// signal. It must NEVER trip the breaker — otherwise one user hitting
// their quota fails fast for everyone, AND the short-circuit masks the
// quota_exceeded error so the client never learns to upgrade its plan.
func TestCircuitBreaker_QuotaErrorNeverOpens(t *testing.T) {
	cb := NewCircuitBreakerPlugin(3, 1*time.Second, 1*time.Second)
	status := 429
	for i := 0; i < 20; i++ {
		ctx := newCtxForTest()
		_, sc, _ := cb.PreLLMHook(ctx, chatReq("openai"))
		if sc != nil {
			t.Fatalf("quota 429 must never short-circuit (i=%d)", i)
		}
		cb.PostLLMHook(ctx, nil, &schemas.BifrostError{
			StatusCode: &status,
			Error:      &schemas.ErrorField{Message: "quota exceeded"},
		})
	}
	if _, sc, _ := cb.PreLLMHook(newCtxForTest(), chatReq("openai")); sc != nil {
		t.Fatal("circuit must stay CLOSED under a flood of 429 quota errors")
	}
}

// Every 4xx is a client-side outcome (bad request, auth, not-found),
// not a sick provider — none of them may open the circuit.
func TestCircuitBreaker_ClientErrorsNeverOpen(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422, 429} {
		cb := NewCircuitBreakerPlugin(2, 1*time.Second, 1*time.Second)
		sc4 := status
		for i := 0; i < 5; i++ {
			ctx := newCtxForTest()
			cb.PreLLMHook(ctx, chatReq("p"))
			cb.PostLLMHook(ctx, nil, &schemas.BifrostError{
				StatusCode: &sc4,
				Error:      &schemas.ErrorField{Message: "client error"},
			})
		}
		if _, sc, _ := cb.PreLLMHook(newCtxForTest(), chatReq("p")); sc != nil {
			t.Fatalf("status %d must not open the circuit", status)
		}
	}
}

// 5xx and transport failures (no HTTP status) DO signal an unhealthy
// provider and must still open the circuit — the resilience guarantee.
func TestCircuitBreaker_ServerAndTransportErrorsOpen(t *testing.T) {
	// 503 upstream error.
	cb := NewCircuitBreakerPlugin(2, 1*time.Second, 1*time.Second)
	s503 := 503
	for i := 0; i < 2; i++ {
		ctx := newCtxForTest()
		cb.PreLLMHook(ctx, chatReq("p"))
		cb.PostLLMHook(ctx, nil, &schemas.BifrostError{
			StatusCode: &s503,
			Error:      &schemas.ErrorField{Message: "upstream down"},
		})
	}
	if _, sc, _ := cb.PreLLMHook(newCtxForTest(), chatReq("p")); sc == nil {
		t.Fatal("503 must open the circuit")
	}

	// Transport failure: no StatusCode at all (dial refused / timeout).
	cb2 := NewCircuitBreakerPlugin(2, 1*time.Second, 1*time.Second)
	for i := 0; i < 2; i++ {
		ctx := newCtxForTest()
		cb2.PreLLMHook(ctx, chatReq("q"))
		cb2.PostLLMHook(ctx, nil, &schemas.BifrostError{Error: &schemas.ErrorField{Message: "connection refused"}})
	}
	if _, sc, _ := cb2.PreLLMHook(newCtxForTest(), chatReq("q")); sc == nil {
		t.Fatal("transport failure (status 0) must open the circuit")
	}
}

func TestCircuitBreaker_DoesNotOpenForSuccess(t *testing.T) {
	cb := NewCircuitBreakerPlugin(2, 1*time.Second, 1*time.Second)
	for i := 0; i < 10; i++ {
		ctx := newCtxForTest()
		cb.PreLLMHook(ctx, chatReq("ok"))
		cb.PostLLMHook(ctx, &schemas.BifrostResponse{}, nil)
	}
	if _, sc, _ := cb.PreLLMHook(newCtxForTest(), chatReq("ok")); sc != nil {
		t.Fatal("success calls must not open the circuit")
	}
}

func TestCircuitBreaker_PerProviderIsolation(t *testing.T) {
	cb := NewCircuitBreakerPlugin(2, 1*time.Second, 1*time.Second)
	// Provider A : fail twice → open
	for i := 0; i < 2; i++ {
		ctx := newCtxForTest()
		cb.PreLLMHook(ctx, chatReq("A"))
		cb.PostLLMHook(ctx, nil, &schemas.BifrostError{Error: &schemas.ErrorField{Message: "fail"}})
	}
	// Provider B : never fails.
	for i := 0; i < 5; i++ {
		ctx := newCtxForTest()
		cb.PreLLMHook(ctx, chatReq("B"))
		cb.PostLLMHook(ctx, &schemas.BifrostResponse{}, nil)
	}
	// A blocked, B OK.
	if _, sc, _ := cb.PreLLMHook(newCtxForTest(), chatReq("A")); sc == nil {
		t.Fatal("A must be open")
	}
	if _, sc, _ := cb.PreLLMHook(newCtxForTest(), chatReq("B")); sc != nil {
		t.Fatal("B must NOT be affected by A's circuit")
	}
}

// ---------- Plugin set wiring ----------

func TestPluginSet_AsLLMPlugins_ReturnsAllThree(t *testing.T) {
	ps := NewDefaultPluginSet(slog.Default(), false)
	list := ps.AsLLMPlugins()
	if len(list) != 3 {
		t.Fatalf("expected 3 plugins, got %d", len(list))
	}
	names := map[string]bool{}
	for _, p := range list {
		names[p.GetName()] = true
	}
	for _, want := range []string{"digitorn.audit", "digitorn.metrics", "digitorn.circuit_breaker"} {
		if !names[want] {
			t.Errorf("missing plugin %s in set", want)
		}
	}
}
