// Package contextsvc is the context subsystem's read model : the single place
// that answers "how full is this session's context window, right now". It
// consolidates logic that used to be scattered (window resolution, the
// output_reserved budget, the occupancy gauge, the pressure ratio) behind one
// stable type — Snapshot — so the engine, hooks, and (future) template
// variables / REST all read the same numbers.
//
// It owns NO state : the per-session occupancy gauge already lives in the
// sessionstore projection (sharded + lock-free, R9/HK-4), so Resolve is a PURE
// function of a SessionSnapshot + the agent's brain. That keeps a single source
// of truth and makes the read O(1) — field loads + arithmetic, never a message
// scan — which is what keeps the turn loop's pressure check at nanosecond cost.
package contextsvc

import (
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime/contextcompact"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// DefaultOutputReserved mirrors the documented default reply headroom
// (docs context-management : output_reserved). Pressure is measured against the
// usable INPUT budget (window − reserve) so compaction fires before the answer
// no longer fits.
const DefaultOutputReserved = 4096

// Snapshot is the stable read model of a session's context window state. It is
// the contract every consumer reads — the engine's pressure check today, and
// {{context.*}} template variables / GET /sessions/{id}/context tomorrow.
type Snapshot struct {
	// TokensUsed is the occupancy : how full the window is now, in tokens.
	// The provider anchor (last LLM round's prompt+completion) when present,
	// else 0 (no anchor yet). NEVER cumulative cost.
	TokensUsed int `json:"tokens_used"`
	// MaxTokens is the usable INPUT budget = Window − OutputReserved (the
	// denominator of Pressure).
	MaxTokens int `json:"max_tokens"`
	// Window is the model's raw context window (before reserving reply room).
	Window int `json:"window"`
	// OutputReserved is the reply headroom carved out of Window.
	OutputReserved int `json:"output_reserved"`
	// Pressure is TokensUsed / MaxTokens in [0,1+]; 0 when MaxTokens<=0 or no
	// anchor. This is exactly what context_pressure(threshold) compares.
	Pressure float64 `json:"pressure"`
	// Remaining is MaxTokens − TokensUsed, floored at 0.
	Remaining int `json:"remaining"`
	// HasAnchor is true once the provider has reported real usage (else the
	// occupancy is an unknown 0, not "the window is empty").
	HasAnchor bool `json:"has_anchor"`
	// CutoffSeq / Strategy describe the active compaction view (0 / "" when
	// none). Messages with Seq <= CutoffSeq are hidden from the model.
	CutoffSeq uint64 `json:"cutoff_seq,omitempty"`
	Strategy  string `json:"strategy,omitempty"`
	// CompactionInflight is true while a compaction is running.
	CompactionInflight bool `json:"compaction_inflight,omitempty"`
}

// Resolve computes the context read model from a session snapshot and the
// agent's brain. Pure + O(1) : no message iteration, no tokenisation. Safe to
// call on the hot path every turn. A zero-value brain resolves the window via
// the provider/model default table.
func Resolve(snap sessionstore.SessionSnapshot, brain schema.Brain) Snapshot {
	return ResolveWithRuntime(snap, brain, 0)
}

// ResolveWithRuntime is Resolve with an explicit runtime-level window hint.
// Precedence: brain.context.max_tokens → runtimeMaxTokens → model table.
func ResolveWithRuntime(snap sessionstore.SessionSnapshot, brain schema.Brain, runtimeMaxTokens int) Snapshot {
	return ResolveWithRuntimeAndGateway(snap, brain, runtimeMaxTokens, 0)
}

// ResolveWithRuntimeAndGateway resolves the context window with the full priority chain:
//  1. brain.context.max_tokens  (explicit per-agent YAML config)
//  2. gatewayWindow             (live from /v1/models max_context_tokens — changes with model switch)
//  3. runtimeMaxTokens          (app-level runtime.context.max_tokens — general ceiling)
//  4. ContextWindowFor()        (hardcoded table — last resort for unknown models)
//
// gatewayWindow wins over runtimeMaxTokens so switching to a smaller-window model
// automatically caps the budget and triggers proactive compaction before overflow.
func ResolveWithRuntimeAndGateway(snap sessionstore.SessionSnapshot, brain schema.Brain, runtimeMaxTokens, gatewayWindow int) Snapshot {
	window := 0
	reserved := 0
	if brain.Context != nil {
		window = brain.Context.MaxTokens
		reserved = brain.Context.OutputReserved
	}
	if window <= 0 && gatewayWindow > 0 {
		window = gatewayWindow
	}
	if window <= 0 && runtimeMaxTokens > 0 {
		window = runtimeMaxTokens
	}
	if window <= 0 {
		window = contextcompact.ContextWindowFor(brain.Provider, brain.Model)
	}
	if reserved <= 0 {
		reserved = DefaultOutputReserved
	}
	maxTok := window
	if maxTok > reserved {
		maxTok -= reserved
	}

	used := snap.ContextTokens
	s := Snapshot{
		TokensUsed:         used,
		MaxTokens:          maxTok,
		Window:             window,
		OutputReserved:     reserved,
		HasAnchor:          used > 0,
		CompactionInflight: snap.CompactionInflight,
	}
	if snap.ContextCompaction != nil {
		s.CutoffSeq = snap.ContextCompaction.CutoffSeq
		s.Strategy = snap.ContextCompaction.Strategy
	}
	if maxTok > 0 {
		s.Pressure = float64(used) / float64(maxTok)
		if r := maxTok - used; r > 0 {
			s.Remaining = r
		}
	}
	return s
}
