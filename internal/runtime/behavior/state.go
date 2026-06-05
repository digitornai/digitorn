package behavior

const toolHistoryCap = 50

type toolHist struct {
	tool   string
	target string
}

// SessionState is the per-session mutable behavior state. A session's turn is
// single-flighted by the runtime (≤1 turn/session at a time), so the fields
// here are mutated by one goroutine at a time ; the engine's mutex guards only
// the session map, not these fields.
type SessionState struct {
	Turn              int
	ViolationsCount   int
	LastViolation     string
	ToolCallsThisTurn int
	TotalToolCalls    int
	LastToolName      string
	ConsecutiveSame   int
	PlanStated        bool

	sets     map[string]map[string]struct{}
	counters map[string]int
	flags    map[string]bool
	history  []toolHist

	// activeProfile is the per-session profile override applied by the
	// composer mode. Empty = the YAML-declared profile. ruleDefs / rules are
	// the session's resolved rule set + resolved profile map ; nil means "use
	// the engine default". Holding these per-session (not on the engine) is
	// the fix for the reference daemon's global-clobber bug, where one
	// session's profile swap rewrote every session's active rules.
	activeProfile string
	ruleDefs      []ruleDef
	rules         map[string]any
}

func newSessionState() *SessionState {
	return &SessionState{
		sets:     map[string]map[string]struct{}{},
		counters: map[string]int{},
		flags:    map[string]bool{},
	}
}

func (s *SessionState) onNewTurn() {
	s.PlanStated = false
	s.ToolCallsThisTurn = 0
	s.Turn++
}

func (s *SessionState) onToolCall(tool, target string) {
	s.ToolCallsThisTurn++
	s.TotalToolCalls++
	s.history = append(s.history, toolHist{tool: tool, target: target})
	if len(s.history) > toolHistoryCap {
		s.history = s.history[len(s.history)-toolHistoryCap:]
	}
	if tool == s.LastToolName {
		s.ConsecutiveSame++
	} else {
		s.LastToolName = tool
		s.ConsecutiveSame = 1
	}
}

func (s *SessionState) addToSet(name, value string) {
	m := s.sets[name]
	if m == nil {
		m = map[string]struct{}{}
		s.sets[name] = m
	}
	m[value] = struct{}{}
}

func (s *SessionState) inSet(name, value string) bool {
	if m := s.sets[name]; m != nil {
		_, ok := m[value]
		return ok
	}
	return false
}

func (s *SessionState) setLen(name string) int { return len(s.sets[name]) }

func (s *SessionState) incrCounter(name string)  { s.counters[name]++ }
func (s *SessionState) resetCounter(name string) { s.counters[name] = 0 }
func (s *SessionState) counter(name string) int  { return s.counters[name] }

func (s *SessionState) setFlag(name string, v bool) { s.flags[name] = v }
func (s *SessionState) flag(name string) bool       { return s.flags[name] }

// snapshot flattens the state into the generic map the classifier consumes
// (counters prefixed "counter:", flags "flag:", non-empty sets by name, plus
// the last 10 tool calls as "recent_tools"). Mirrors the reference daemon.
func (s *SessionState) snapshot() map[string]any {
	snap := map[string]any{
		"turn":                  s.Turn,
		"total_tool_calls":      s.TotalToolCalls,
		"tool_calls_this_turn":  s.ToolCallsThisTurn,
		"consecutive_same_tool": s.ConsecutiveSame,
		"last_tool_name":        s.LastToolName,
		"violations_count":      s.ViolationsCount,
		"last_violation":        s.LastViolation,
	}
	for name, set := range s.sets {
		if len(set) == 0 {
			continue
		}
		vals := make([]any, 0, len(set))
		for v := range set {
			vals = append(vals, v)
		}
		snap[name] = vals
	}
	for name, c := range s.counters {
		if c != 0 {
			snap["counter:"+name] = c
		}
	}
	for name, f := range s.flags {
		if f {
			snap["flag:"+name] = true
		}
	}
	if len(s.history) > 0 {
		recent := s.history
		if len(recent) > 10 {
			recent = recent[len(recent)-10:]
		}
		names := make([]string, 0, len(recent))
		for _, h := range recent {
			if h.target != "" {
				names = append(names, h.tool+"("+h.target+")")
			} else {
				names = append(names, h.tool)
			}
		}
		snap["recent_tools"] = names
	}
	return snap
}
