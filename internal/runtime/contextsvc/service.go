package contextsvc

import (
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/contextcompact"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

const DefaultOutputReserved = 4096

type Snapshot struct {
	TokensUsed int `json:"tokens_used"`
	MaxTokens int `json:"max_tokens"`
	Window int `json:"window"`
	OutputReserved int `json:"output_reserved"`
	Pressure float64 `json:"pressure"`
	Remaining int `json:"remaining"`
	HasAnchor bool `json:"has_anchor"`
	CutoffSeq uint64 `json:"cutoff_seq,omitempty"`
	Strategy  string `json:"strategy,omitempty"`
	CompactionInflight bool `json:"compaction_inflight,omitempty"`
}

func Resolve(snap sessionstore.SessionSnapshot, brain schema.Brain) Snapshot {
	return ResolveWithRuntime(snap, brain, 0)
}

func ResolveWithRuntime(snap sessionstore.SessionSnapshot, brain schema.Brain, runtimeMaxTokens int) Snapshot {
	return ResolveWithRuntimeAndGateway(snap, brain, runtimeMaxTokens, 0)
}

func ResolveWithRuntimeAndGateway(snap sessionstore.SessionSnapshot, brain schema.Brain, runtimeMaxTokens, gatewayWindow int) Snapshot {
	window := 0
	reserved := 0
	if brain.Context != nil {
		reserved = brain.Context.OutputReserved
	}
	if gatewayWindow > 0 {
		window = gatewayWindow
	} else if brain.Context != nil && brain.Context.MaxTokens > 0 {
		window = brain.Context.MaxTokens
	}
	if window <= 0 && runtimeMaxTokens > 0 {
		window = runtimeMaxTokens
	}
	if window <= 0 {
		window = contextcompact.DefaultContextWindow
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
