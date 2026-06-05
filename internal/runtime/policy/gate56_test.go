package policy

import (
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

func TestGate5_Classification(t *testing.T) {
	cases := []struct {
		name, action, max string
		wantDeny          bool
	}{
		{"over", "restricted", "internal", true},
		{"equal", "internal", "internal", false},
		{"under", "public", "confidential", false},
		{"no-cap", "restricted", "", false},
		{"no-action", "", "public", false},
		{"unknown-action-defaults-internal-over-public", "weird", "public", true}, // unknown→1 > public 0
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Gate5Classification(
				Invocation{Module: "m", Action: "a"},
				PolicyContext{
					Capabilities: &schema.CapabilitiesConfig{MaxDataClassification: c.max},
					ToolSpec:     &tool.Spec{DataClassification: c.action},
				})
			if c.wantDeny {
				if d.Kind != DecisionDeny || d.Gate != GateClassification {
					t.Fatalf("want gate5 deny, got %+v", d)
				}
			} else if d.Kind != DecisionAllow {
				t.Fatalf("want allow, got %+v", d)
			}
		})
	}
	if Gate5Classification(Invocation{}, PolicyContext{}).Kind != DecisionAllow {
		t.Error("nil spec/caps must allow")
	}
}

func TestRateLimiter_SlidingWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	lim := NewRateLimiter(map[string]int{"m.a": 2})
	lim.now = func() time.Time { return now }

	if lim.Check("m", "a") != "" || lim.Check("m", "a") != "" {
		t.Fatal("first 2 calls must be allowed")
	}
	if lim.Check("m", "a") == "" {
		t.Fatal("3rd call must be rate-limited")
	}
	// Advance past the window → budget refills.
	now = now.Add(61 * time.Second)
	if lim.Check("m", "a") != "" {
		t.Fatal("after the window the call must be allowed again")
	}
	// An unconfigured key with no "*" default is unlimited.
	for i := 0; i < 5; i++ {
		if lim.Check("m", "b") != "" {
			t.Fatalf("unconfigured key must be unlimited (#%d)", i)
		}
	}
}

func TestRateLimiter_DefaultAndUnlimitedAndNil(t *testing.T) {
	now := time.Unix(0, 0)
	lim := NewRateLimiter(map[string]int{"*": 1, "m.free": 0})
	lim.now = func() time.Time { return now }

	if lim.Check("x", "y") != "" {
		t.Fatal("first via default must be allowed")
	}
	if lim.Check("x", "y") == "" {
		t.Fatal("second via default(1) must be limited")
	}
	// Explicit 0 = unlimited even when a "*" default exists.
	for i := 0; i < 5; i++ {
		if lim.Check("m", "free") != "" {
			t.Fatalf("explicit-0 key must be unlimited (#%d)", i)
		}
	}
	// NewRateLimiter(empty) is nil → Check is a no-op.
	if NewRateLimiter(nil) != nil {
		t.Fatal("empty limits must yield nil limiter")
	}
	var nilLim *RateLimiter
	if nilLim.Check("a", "b") != "" {
		t.Fatal("nil limiter must allow")
	}
}

func TestRunGates_Gate6RuntimeOnly(t *testing.T) {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, MaxRiskLevel: schema.RiskLevel(tool.RiskHigh)}
	inv := Invocation{Caller: CallerLLM, Module: "m", Action: "a"}
	spec := &tool.Spec{Name: "m.a", RiskLevel: tool.RiskLow}

	lim := NewRateLimiter(map[string]int{"m.a": 1})
	now := time.Unix(500, 0)
	lim.now = func() time.Time { return now }

	pc := PolicyContext{AppActive: true, Capabilities: caps, ToolSpec: spec, RateLimiter: lim}
	if d := RunGates(inv, pc); d.Kind != DecisionAllow {
		t.Fatalf("1st call must pass, got %+v", d)
	}
	if d := RunGates(inv, pc); d.Kind != DecisionDeny || d.Gate != GateRateLimit {
		t.Fatalf("2nd call must be gate6 deny, got %+v", d)
	}
	// Schema-build path : no limiter wired → gate 6 never fires.
	pcNoLim := PolicyContext{AppActive: true, Capabilities: caps, ToolSpec: spec}
	for i := 0; i < 5; i++ {
		if RunGates(inv, pcNoLim).Kind != DecisionAllow {
			t.Fatalf("without a limiter every call must pass (#%d)", i)
		}
	}
}

func TestRunGates_DeniedCallDoesNotConsumeRateBudget(t *testing.T) {
	// gate 2 denies (risk over max) BEFORE gate 6 runs, so the rate window
	// must NOT count the denied attempts.
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto, MaxRiskLevel: schema.RiskLevel(tool.RiskLow)}
	inv := Invocation{Caller: CallerLLM, Module: "m", Action: "a"}
	spec := &tool.Spec{Name: "m.a", RiskLevel: tool.RiskHigh} // over max → gate2 deny

	lim := NewRateLimiter(map[string]int{"m.a": 1})
	now := time.Unix(0, 0)
	lim.now = func() time.Time { return now }
	pc := PolicyContext{AppActive: true, Capabilities: caps, ToolSpec: spec, RateLimiter: lim}

	for i := 0; i < 5; i++ {
		if d := RunGates(inv, pc); d.Kind != DecisionDeny || d.Gate != GateRisk {
			t.Fatalf("expected gate2 deny (#%d), got %+v", i, d)
		}
	}
	// Now make it pass gate 2 : the limit-1 budget must still be intact.
	spec.RiskLevel = tool.RiskLow
	if d := RunGates(inv, pc); d.Kind != DecisionAllow {
		t.Fatalf("denied calls must not consume rate budget, got %+v", d)
	}
}
