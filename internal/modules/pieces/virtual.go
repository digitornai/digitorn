package pieces

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

var (
	highRiskRe = regexp.MustCompile(`(?:^|_)(?:delete|drop|destroy|remove|kill|purge|truncate|wipe|revoke)(?:_|$)`)
	lowRiskRe  = regexp.MustCompile(`(?:^|_)(?:get|list|search|read|describe|count|fetch|check|browse|view|show|info|status|ping|health|find|retrieve)(?:_|$)`)
)

// parseAPTool splits "ap_{piece}__{action}" into piece and action.
func parseAPTool(name string) (piece, action string, ok bool) {
	if !strings.HasPrefix(name, "ap_") {
		return
	}
	rest := name[3:]
	p, a, ok2 := strings.Cut(rest, "__")
	if !ok2 {
		return
	}
	return p, a, true
}

func inferRisk(name string) (tool.RiskLevel, bool) {
	n := strings.ToLower(name)
	if highRiskRe.MatchString(n) {
		return tool.RiskHigh, true
	}
	if lowRiskRe.MatchString(n) {
		return tool.RiskLow, false
	}
	return tool.RiskMedium, false
}

// pieceTagOf extracts the piece name from "ap_{piece}__{action}" for tagging.
func pieceTagOf(toolName string) string {
	piece, _, ok := parseAPTool(toolName)
	if !ok {
		return ""
	}
	return piece
}

// schemaToParams converts an MCP inputSchema map to []tool.ParamSpec.
// Mirrors the MCP module's implementation.
func schemaToParams(schema map[string]any) []tool.ParamSpec {
	if schema == nil {
		return nil
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var s struct {
		Properties map[string]struct {
			Type        any    `json:"type"`
			Description string `json:"description"`
			Enum        []any  `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if json.Unmarshal(b, &s) != nil || len(s.Properties) == 0 {
		return nil
	}
	req := make(map[string]bool, len(s.Required))
	for _, r := range s.Required {
		req[r] = true
	}
	// Skip internal bridge params from the agent's view.
	skip := map[string]bool{"_ap_auth": true, "_ap_session": true}
	out := make([]tool.ParamSpec, 0, len(s.Properties))
	for name, p := range s.Properties {
		if skip[name] {
			continue
		}
		out = append(out, tool.ParamSpec{
			Name:        name,
			Type:        schemaType(p.Type),
			Description: p.Description,
			Required:    req[name],
			Enum:        p.Enum,
		})
	}
	return out
}

func schemaType(t any) string {
	switch v := t.(type) {
	case string:
		return v
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s != "null" {
				return s
			}
		}
	}
	return "string"
}
