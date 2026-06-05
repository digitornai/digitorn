package tool

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Call is an LLM-issued tool invocation request.
type Call struct {
	ID       string          `json:"id"`
	ModuleID string          `json:"module_id"`
	ToolName string          `json:"tool_name"`
	Params   json.RawMessage `json:"params"`
}

// CallRecord is the persisted record of a tool call, used for audit/replay.
type CallRecord struct {
	ID         uuid.UUID       `json:"id"`
	SessionID  uuid.UUID       `json:"session_id"`
	TurnNumber int             `json:"turn_number"`
	ModuleID   string          `json:"module_id"`
	ToolName   string          `json:"tool_name"`
	Params     json.RawMessage `json:"params"`
	Result     Result          `json:"result"`
	ExecutedAt time.Time       `json:"executed_at"`
	DurationMs int64           `json:"duration_ms"`
}
