package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// resolveAgent picks the agent that runs a turn. Precedence :
//
//  1. an explicit logical id (in.AgentID) — set by the AgentManager when
//     running an isolated sub-agent turn ; nil if that id isn't declared so
//     the caller surfaces a clean error.
//  2. runtime.entry_agent — the app's designated coordinator / front door.
//  3. the first declared agent — the single-agent default.
//
// The caller has already guaranteed len(def.Agents) > 0.
func resolveAgent(def *schema.AppDefinition, agentID string) *schema.Agent {
	if agentID != "" {
		for i := range def.Agents {
			if def.Agents[i].ID == agentID {
				return &def.Agents[i]
			}
		}
		return nil
	}
	if def.Runtime != nil && def.Runtime.EntryAgent != "" {
		for i := range def.Agents {
			if def.Agents[i].ID == def.Runtime.EntryAgent {
				return &def.Agents[i]
			}
		}
	}
	return &def.Agents[0]
}

// applyEntryAgent honors a per-session entry-agent override (set at session
// creation by a non-human launcher, e.g. a background channel trigger). An
// explicit caller agent (sub-agent runs set explicitID) still wins; an empty or
// non-existent sessionEntry leaves the already-resolved agent untouched, so a
// bad value can never break the session.
func applyEntryAgent(def *schema.AppDefinition, current *schema.Agent, explicitID, sessionEntry string) *schema.Agent {
	if explicitID != "" || sessionEntry == "" {
		return current
	}
	if a := resolveAgent(def, sessionEntry); a != nil {
		return a
	}
	return current
}

// agentRunID returns the distinct per-instance identity attributed to the
// gateway/provider + telemetry : the explicit run id when set (sub-agents),
// else the agent's stable logical id (the entry agent).
func agentRunID(runID, logicalID string) string {
	if runID != "" {
		return runID
	}
	return logicalID
}

// NewAgentRunID builds a distinct run id for a freshly spawned agent instance,
// of the form "<logicalID>#<short>" so two concurrent instances of the same
// specialist are distinguishable on the wire and in telemetry, while the
// logical agent stays readable as the prefix.
func NewAgentRunID(logicalID string) string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	suffix := hex.EncodeToString(b[:])
	if logicalID == "" {
		return "agent#" + suffix
	}
	// Defensive : if a run id is somehow re-derived, keep a single logical
	// prefix rather than nesting "a#x#y".
	if i := strings.IndexByte(logicalID, '#'); i >= 0 {
		logicalID = logicalID[:i]
	}
	return logicalID + "#" + suffix
}
