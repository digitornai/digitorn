package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

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
	if len(def.Agents) == 0 {
		return nil
	}
	return &def.Agents[0]
}

func applyEntryAgent(def *schema.AppDefinition, current *schema.Agent, explicitID, sessionEntry string) *schema.Agent {
	if explicitID != "" || sessionEntry == "" {
		return current
	}
	if a := resolveAgent(def, sessionEntry); a != nil {
		return a
	}
	return current
}

func agentRunID(runID, logicalID string) string {
	if runID != "" {
		return runID
	}
	return logicalID
}

func NewAgentRunID(logicalID string) string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	suffix := hex.EncodeToString(b[:])
	if logicalID == "" {
		return "agent#" + suffix
	}
	if i := strings.IndexByte(logicalID, '#'); i >= 0 {
		logicalID = logicalID[:i]
	}
	return logicalID + "#" + suffix
}
