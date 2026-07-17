package module

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
)

type Base struct {
	ID                  string
	Version             string
	Description         string
	SupportedPlatforms  []domainmodule.Platform
	Dependencies        []string
	DeclaredPermissions []string
	ProvidesServices    []string
	ConsumesServices    []string
	Constraints         []ConstraintSpec
	ConfigSchema        map[string]any

	mu         sync.RWMutex
	tools      map[string]Tool
	middleware []ToolMiddleware
	state      stateTracker
	config     any
}

func (b *Base) RegisterTool(t Tool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tools == nil {
		b.tools = make(map[string]Tool)
	}
	b.tools[t.Name] = t
}

func (b *Base) Tools() []Tool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Tool, 0, len(b.tools))
	for _, t := range b.tools {
		out = append(out, t)
	}
	return out
}

func (b *Base) State() State { return b.state.get() }

func (b *Base) Manifest() domainmodule.Manifest {
	b.mu.RLock()
	specs := make([]tool.Spec, 0, len(b.tools))
	for _, t := range b.tools {
		specs = append(specs, t.toSpec())
	}
	b.mu.RUnlock()
	return domainmodule.Manifest{
		ID:                  b.ID,
		Version:             b.Version,
		Description:         b.Description,
		SupportedPlatforms:  b.SupportedPlatforms,
		Tools:               specs,
		Dependencies:        b.Dependencies,
		DeclaredPermissions: b.DeclaredPermissions,
		ProvidesServices:    b.ProvidesServices,
		ConsumesServices:    b.ConsumesServices,
		ConfigSchema:        b.ConfigSchema,
	}
}

func (b *Base) BindConfig(cfg map[string]any, target any) error {
	if cfg == nil {
		return nil
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("module %q: marshal config: %w", b.ID, err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("module %q: decode config: %w", b.ID, err)
	}
	b.mu.Lock()
	b.config = target
	b.mu.Unlock()
	return nil
}

func (b *Base) Config() any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.config
}

func (b *Base) Init(ctx context.Context, cfg map[string]any) error { return nil }

func (b *Base) Start(ctx context.Context) error {
	if err := b.state.transition(StateStarting); err != nil {
		return err
	}
	return b.state.transition(StateActive)
}

func (b *Base) Stop(ctx context.Context) error {
	if err := b.state.transition(StateStopping); err != nil {
		return err
	}
	return b.state.transition(StateDisabled)
}

func (b *Base) Pause(ctx context.Context) error {
	return b.state.transition(StatePaused)
}

func (b *Base) Resume(ctx context.Context) error {
	return b.state.transition(StateActive)
}

func (b *Base) UpdateConfig(ctx context.Context, cfg map[string]any) error {
	b.mu.RLock()
	target := b.config
	b.mu.RUnlock()
	if target == nil {
		return nil
	}
	return b.BindConfig(cfg, target)
}

func (b *Base) Invoke(ctx context.Context, name string, params []byte) (tool.Result, error) {
	b.mu.RLock()
	t, ok := b.tools[name]
	b.mu.RUnlock()
	if !ok {
		return tool.Result{Success: false, Error: fmt.Sprintf("unknown tool %q", name)},
			fmt.Errorf("module %q: unknown tool %q", b.ID, name)
	}
	if t.Handler == nil {
		return tool.Result{Success: false, Error: "tool has no handler"},
			fmt.Errorf("module %q: tool %q has nil handler", b.ID, name)
	}
	if params == nil {
		params = json.RawMessage("{}")
	}
	id, _ := tool.IdentityFromContext(ctx)
	id.ModuleID, id.ToolName = b.ID, name
	ctx = tool.WithIdentity(ctx, id)
	raw := json.RawMessage(params)
	if err := enforceConstraints(ctx, b.ID, name, raw); err != nil {
		return resultFor(err), err
	}
	release, err := applyGuards(ctx, b.ID, name, raw)
	if err != nil {
		return resultFor(err), err
	}
	defer release()
	handler := b.wrapHandler(t.Handler)
	return handler(ctx, raw)
}

var (
	_ domainmodule.Module   = (*Base)(nil)
	_ domainmodule.Pauser   = (*Base)(nil)
	_ domainmodule.Reloader = (*Base)(nil)
)
