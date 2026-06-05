package middlewareplugin

import (
	"context"
	"encoding/json"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// BeforeFunc / AfterFunc are the plugin author's hooks. Before may mutate the
// Context (SystemPrompt / Messages) in place and return (response,
// shortCircuit). After transforms the response.
type (
	BeforeFunc func(ctx context.Context, c *Context) (response string, shortCircuit bool, err error)
	AfterFunc  func(ctx context.Context, c *Context, response string, toolCalls []ToolCall) (string, error)
)

// Module wraps Before/After funcs into a domainmodule.Module hosting the
// before/after tools, so a Go middleware plugin is a few lines. Host it in a
// worker binary built on the generic worker framework. Non-Go plugins
// implement the same gRPC ModuleService directly.
func Module(id string, before BeforeFunc, after AfterFunc) domainmodule.Module {
	return &pluginModule{id: id, before: before, after: after}
}

type pluginModule struct {
	id     string
	before BeforeFunc
	after  AfterFunc
}

func (m *pluginModule) Manifest() domainmodule.Manifest {
	return domainmodule.Manifest{
		ID: m.id, Version: "1.0.0", Description: "custom app middleware plugin",
		Tools: []tool.Spec{{Name: ToolBefore}, {Name: ToolAfter}},
	}
}

func (m *pluginModule) Init(context.Context, map[string]any) error { return nil }
func (m *pluginModule) Start(context.Context) error                { return nil }
func (m *pluginModule) Stop(context.Context) error                 { return nil }

func (m *pluginModule) Invoke(ctx context.Context, toolName string, params []byte) (tool.Result, error) {
	switch toolName {
	case ToolBefore:
		var req BeforeRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return tool.Result{Success: false, Error: "bad before params: " + err.Error()}, nil
		}
		c := req.Context
		var (
			resp string
			sc   bool
			err  error
		)
		if m.before != nil {
			resp, sc, err = m.before(ctx, &c)
		}
		if err != nil {
			return tool.Result{Success: false, Error: err.Error()}, nil
		}
		return tool.Result{Success: true, Data: BeforeResult{
			SystemPrompt: c.SystemPrompt, Messages: c.Messages, Response: resp, ShortCircuit: sc,
		}}, nil

	case ToolAfter:
		var req AfterRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return tool.Result{Success: false, Error: "bad after params: " + err.Error()}, nil
		}
		out := req.Response
		var err error
		if m.after != nil {
			out, err = m.after(ctx, &req.Context, req.Response, req.ToolCalls)
		}
		if err != nil {
			return tool.Result{Success: false, Error: err.Error()}, nil
		}
		return tool.Result{Success: true, Data: AfterResult{Response: out}}, nil
	}
	return tool.Result{Success: false, Error: "unknown tool: " + toolName}, nil
}

var _ domainmodule.Module = (*pluginModule)(nil)
