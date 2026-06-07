package mcp

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	highRiskRe = regexp.MustCompile(`(?:^|_)(?:delete|drop|destroy|remove|kill|purge|truncate|wipe)(?:_|$)`)
	lowRiskRe  = regexp.MustCompile(`(?:^|_)(?:get|list|search|read|describe|count|fetch|check|browse|view|show|info|status|ping|health)(?:_|$)`)
)

var pathParamNames = map[string]bool{
	"path": true, "file_path": true, "filepath": true, "filename": true,
	"dir": true, "directory": true, "folder": true, "cwd": true, "workdir": true,
	"workspace": true, "source": true, "src": true, "target": true, "dst": true,
	"destination": true, "output": true, "input_path": true, "output_path": true,
}

// LiveTools materializes every connected server's tools (+ its prompt/resource
// tools) into native tool.Specs. RiskLevel is always set — an empty risk fails
// the risk gate closed.
func (m *Module) LiveTools(ctx context.Context) []tool.Spec {
	m.ensureConnected(ctx)
	m.mu.RLock()
	pool := m.pool
	m.mu.RUnlock()
	if pool == nil {
		return nil
	}
	var out []tool.Spec
	for _, srv := range pool.live() {
		for _, t := range srv.tools {
			if t != nil {
				out = append(out, virtualToolSpec(srv.id, t))
			}
		}
		if srv.hasPrompts {
			out = append(out, promptTools(srv.id)...)
		}
		if srv.hasResources {
			out = append(out, resourceTools(srv.id)...)
		}
	}
	return out
}

func virtualToolSpec(server string, t *mcpsdk.Tool) tool.Spec {
	risk, irreversible := inferRisk(t.Name)
	desc := strings.TrimSpace(t.Description)
	if desc == "" {
		desc = "Tool " + t.Name + " on MCP server " + server + "."
	}
	return tool.Spec{
		Name:         "mcp_" + server + "__" + t.Name,
		Description:  desc,
		Params:       schemaToParams(t.InputSchema),
		RiskLevel:    risk,
		Irreversible: irreversible,
		Tags:         []string{"mcp", server},
	}
}

func promptTools(server string) []tool.Spec {
	pre := "mcp_" + server + "__"
	tags := []string{"mcp", server, "prompt"}
	return []tool.Spec{
		{Name: pre + "list_prompts", Description: "List the prompt templates offered by MCP server " + server + ".", RiskLevel: tool.RiskLow, Tags: tags},
		{Name: pre + "get_prompt", Description: "Fetch a prompt template from MCP server " + server + ".",
			Params: []tool.ParamSpec{
				{Name: "prompt_name", Type: "string", Description: "Name of the prompt.", Required: true},
				{Name: "arguments", Type: "object", Description: "Template arguments."},
			},
			RiskLevel: tool.RiskLow, Tags: tags},
	}
}

func resourceTools(server string) []tool.Spec {
	pre := "mcp_" + server + "__"
	tags := []string{"mcp", server, "resource"}
	return []tool.Spec{
		{Name: pre + "list_resources", Description: "List the resources offered by MCP server " + server + ".", RiskLevel: tool.RiskLow, Tags: tags},
		{Name: pre + "read_resource", Description: "Read a resource from MCP server " + server + ".",
			Params: []tool.ParamSpec{
				{Name: "uri", Type: "string", Description: "Resource URI (server-namespaced, not a workspace path).", Required: true, Path: false},
			},
			RiskLevel: tool.RiskLow, Tags: tags},
	}
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

// schemaToParams maps an MCP inputSchema to ParamSpecs via a JSON round-trip,
// marking path params so the workdir policy confines them.
func schemaToParams(input any) []tool.ParamSpec {
	if input == nil {
		return nil
	}
	b, err := json.Marshal(input)
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
	required := make(map[string]bool, len(s.Required))
	for _, r := range s.Required {
		required[r] = true
	}
	out := make([]tool.ParamSpec, 0, len(s.Properties))
	for name, p := range s.Properties {
		out = append(out, tool.ParamSpec{
			Name:        name,
			Type:        schemaType(p.Type),
			Description: p.Description,
			Required:    required[name],
			Enum:        p.Enum,
			Path:        pathParamNames[strings.ToLower(name)],
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
