package contextsvc

import (
	"sync"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// ContextView is the first-class context variable : the single, rich read-model
// of a session's context that BOTH the internal pipeline (the context_pressure
// hook, compaction trigger, turn logic) AND app developers read as `context.*`.
// It is kept as close to the real context as possible by the background Context
// Service : every recompute refreshes it, so a hook condition (`context.pressure
// > 0.6`) or a template (`{{context.used}}/{{context.window}}`) always sees a
// value that tracks the true token count.
type ContextView struct {
	// --- occupancy & window ---
	Used           int     `json:"used"`            // exact tokens in the window now
	Window         int     `json:"window"`          // model's raw context window
	Limit          int     `json:"limit"`           // usable input budget (window − output_reserved)
	OutputReserved int     `json:"output_reserved"` // reply headroom carved out of the window
	Remaining      int     `json:"remaining"`       // limit − used, floored at 0
	Pressure       float64 `json:"pressure"`        // used / limit (0..1+)
	PressurePct    int     `json:"pressure_pct"`    // pressure as a whole percentage
	HasAnchor      bool    `json:"has_anchor"`      // a real provider/tokenizer count exists

	// --- breakdown (the three budget buckets) ---
	System      int `json:"system"`
	Tools       int `json:"tools"`
	Messages    int `json:"messages"`
	SystemPct   int `json:"system_pct"`
	ToolsPct    int `json:"tools_pct"`
	MessagesPct int `json:"messages_pct"`

	// --- compaction ---
	Compacting      bool   `json:"compacting"`       // a compaction is in flight
	Compacted       bool   `json:"compacted"`        // a compaction view is active
	Compactions     int    `json:"compactions"`      // count this session
	CutoffSeq       uint64 `json:"cutoff_seq"`       // active compaction cutoff
	Strategy        string `json:"strategy"`         // active strategy (truncate / summarize)
	MessagesDropped int    `json:"messages_dropped"` // messages elided from the view

	// --- conversation & turn ---
	Turns        int `json:"turns"`         // turns so far
	MessageCount int `json:"message_count"` // messages in the session
	ToolCalls    int `json:"tool_calls"`    // tool calls so far
	Round        int `json:"round"`         // current LLM round within the turn

	// --- cost (cumulative, distinct from occupancy) ---
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`

	// --- model, source & freshness ---
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Exact      bool   `json:"exact"`       // true = tokenizer count ; false = anchor-only/estimate
	Source     string `json:"source"`      // "tokenizer" | "anchor" | "stream"
	UpdatedSeq uint64 `json:"updated_seq"` // session seq at last refresh (freshness proof)

	// --- live (during generation) ---
	LiveOutput int `json:"live_output"` // running output tokens of the in-flight reply

	// EstimateRatio is the self-calibrated provider-tokens / local-estimate ratio
	// for this session's model, learned from each round's exact prompt_tokens. It
	// lets the pre-send budget guard target the model's REAL count even when no
	// local tokenizer matches it (Claude/Gemini). 0 = not yet calibrated.
	EstimateRatio float64 `json:"estimate_ratio,omitempty"`

	// TokRatio calibrates the background tokenizer (tiktoken) count to THIS
	// session's provider real count : provider_tokens / tokenizer_tokens for the
	// same content, learned at each turn boundary and applied to every recount so
	// the displayed occupancy tracks the provider for ANY provider, not tiktoken.
	// 0 = not yet learned (raw tokenizer count used). Distinct from EstimateRatio
	// (which is provider / char-heuristic, for the pre-send budget guard).
	TokRatio float64 `json:"tok_ratio,omitempty"`
}

// ViewFromSnapshot builds the full ContextView from a session projection + the
// agent's brain. Pure + O(1)-ish (one pass over messages only for the dropped
// count). Window/limit/pressure come from Resolve so the variable agrees with
// the compaction denominator exactly. Source/Exact default to the gauge's
// origin ; the caller overlays Round / LiveOutput / a fresher Used when it has
// them (e.g. straight off a tokenizer notification).
func ViewFromSnapshot(snap sessionstore.SessionSnapshot, brain schema.Brain) ContextView {
	return ViewFromSnapshotWithRuntime(snap, brain, 0)
}

// ViewFromSnapshotWithRuntime is ViewFromSnapshot with a runtime-level window
// fallback (runtime.context.max_tokens) used when the brain has no explicit
// max_tokens and the model is not in the built-in window table.
func ViewFromSnapshotWithRuntime(snap sessionstore.SessionSnapshot, brain schema.Brain, runtimeMaxTokens int) ContextView {
	return ViewFromSnapshotWithRuntimeAndGateway(snap, brain, runtimeMaxTokens, 0)
}

func ViewFromSnapshotWithRuntimeAndGateway(snap sessionstore.SessionSnapshot, brain schema.Brain, runtimeMaxTokens, gatewayWindow int) ContextView {
	s := ResolveWithRuntimeAndGateway(snap, brain, runtimeMaxTokens, gatewayWindow)
	v := ContextView{
		Used:           s.TokensUsed,
		Window:         s.Window,
		Limit:          s.MaxTokens,
		OutputReserved: s.OutputReserved,
		Remaining:      s.Remaining,
		Pressure:       s.Pressure,
		HasAnchor:      s.HasAnchor,
		System:         snap.ContextSystemTokens,
		Tools:          snap.ContextToolsTokens,
		Messages:       snap.ContextMessageTokens,
		Compacting:     s.CompactionInflight,
		Compactions:    len(snap.Compactions),
		CutoffSeq:      s.CutoffSeq,
		Strategy:       s.Strategy,
		Turns:          snap.TurnCount,
		MessageCount:   len(snap.Messages),
		ToolCalls:      len(snap.ToolCalls),
		TokensIn:       snap.TokensIn,
		TokensOut:      snap.TokensOut,
		CostUSD:        snap.UsdTotal,
		Provider:       brain.Provider,
		Model:          brain.Model,
		Exact:          s.HasAnchor,
		Source:         "anchor",
		UpdatedSeq:     snap.LastSeq,
	}
	v.Compacted = s.CutoffSeq > 0
	if v.Compacted {
		for i := range snap.Messages {
			if snap.Messages[i].Seq <= s.CutoffSeq {
				v.MessagesDropped++
			}
		}
	}
	v.derive()
	return v
}

// derive fills the computed convenience fields (percentages) from the primaries.
// Called after any field that feeds them is set.
func (v *ContextView) derive() {
	v.PressurePct = int(v.Pressure * 100)
	if v.Used > 0 {
		v.SystemPct = v.System * 100 / v.Used
		v.ToolsPct = v.Tools * 100 / v.Used
		v.MessagesPct = v.Messages * 100 / v.Used
	} else {
		v.SystemPct, v.ToolsPct, v.MessagesPct = 0, 0, 0
	}
	if v.Limit > 0 {
		if r := v.Limit - v.Used; r > 0 {
			v.Remaining = r
		} else {
			v.Remaining = 0
		}
		v.Pressure = float64(v.Used) / float64(v.Limit)
		v.PressurePct = int(v.Pressure * 100)
	}
}

// WithExactTotal overlays a fresher EXACT total (and its breakdown) straight off
// a tokenizer recompute, recomputing the derived fields. Used by the runtime to
// keep the variable ahead of the durable projection round-trip.
func (v ContextView) WithExactTotal(total, system, tools, messages int) ContextView {
	v.Used, v.System, v.Tools, v.Messages = total, system, tools, messages
	v.HasAnchor = total > 0
	v.Exact = true
	v.Source = "tokenizer"
	v.derive()
	return v
}

// Tracker holds the freshest ContextView per session in memory. The runtime
// writes it on every Context Service notification (so it leads the durable
// projection) ; the hook pipeline, the turn loop, and the `context.*` variable
// resolver read it. Lock-free reads (sync.Map) ; per-session isolation is total
// — a key is one session, never shared.
type Tracker struct {
	views sync.Map // sessionID -> ContextView
}

// NewTracker constructs an empty tracker.
func NewTracker() *Tracker { return &Tracker{} }

// Put stores (replaces) the view for a session.
func (t *Tracker) Put(sessionID string, v ContextView) {
	if t == nil || sessionID == "" {
		return
	}
	t.views.Store(sessionID, v)
}

// Get returns the current view for a session. ok=false when nothing was tracked
// yet (the caller falls back to building one from the snapshot).
func (t *Tracker) Get(sessionID string) (ContextView, bool) {
	if t == nil || sessionID == "" {
		return ContextView{}, false
	}
	v, ok := t.views.Load(sessionID)
	if !ok {
		return ContextView{}, false
	}
	return v.(ContextView), true
}

// Delete drops a session's view (session end / eviction) so the map can't leak.
func (t *Tracker) Delete(sessionID string) {
	if t == nil || sessionID == "" {
		return
	}
	t.views.Delete(sessionID)
}
