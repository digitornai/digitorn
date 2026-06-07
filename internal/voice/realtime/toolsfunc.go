package realtime

import (
	"context"
	"errors"
)

// ToolsFunc adapts plain closures to the Tools interface, so the daemon can supply
// its gated toolset + single-tool executor without the realtime package importing the
// engine. The server wires SpecsFn → the curated toolset (Engine.VoiceContext) and
// ExecuteFn → Engine.ExecuteToolGated, keeping this package daemon-agnostic + testable.
type ToolsFunc struct {
	SpecsFn   func() []ToolSpec
	ExecuteFn func(ctx context.Context, callID, name, argsJSON string) (string, error)
}

func (t ToolsFunc) Specs() []ToolSpec {
	if t.SpecsFn == nil {
		return nil
	}
	return t.SpecsFn()
}

func (t ToolsFunc) Execute(ctx context.Context, callID, name, argsJSON string) (string, error) {
	if t.ExecuteFn == nil {
		return "", errors.New("realtime: no tool executor wired")
	}
	return t.ExecuteFn(ctx, callID, name, argsJSON)
}

var _ Tools = ToolsFunc{}
