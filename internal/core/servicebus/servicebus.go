// Package servicebus provides an in-process registry of running modules and
// dispatches inter-module action calls.
package servicebus

import (
	"context"
	"fmt"
	"sync"

	"github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/ports"
)

// Bus is the default in-memory implementation of ports.ServiceBus.
type Bus struct {
	mu      sync.RWMutex
	modules map[string]module.Module
}

// New creates an empty ServiceBus.
func New() *Bus {
	return &Bus{modules: make(map[string]module.Module)}
}

// Register adds (or replaces) a module by its manifest ID.
func (b *Bus) Register(m module.Module) error {
	if m == nil {
		return fmt.Errorf("servicebus: nil module")
	}
	id := m.Manifest().ID
	if id == "" {
		return fmt.Errorf("servicebus: module has empty ID")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.modules[id] = m
	return nil
}

// Unregister removes a module by ID.
func (b *Bus) Unregister(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.modules, id)
	return nil
}

// Get returns the module with the given ID.
func (b *Bus) Get(id string) (module.Module, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	m, ok := b.modules[id]
	return m, ok
}

// List returns all registered modules.
func (b *Bus) List() []module.Module {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]module.Module, 0, len(b.modules))
	for _, m := range b.modules {
		out = append(out, m)
	}
	return out
}

// Call dispatches a tool call. The dispatcher is responsible for any
// permission check; this method is a pure dispatch.
func (b *Bus) Call(ctx context.Context, moduleID, toolName string, params []byte) (tool.Result, error) {
	m, ok := b.Get(moduleID)
	if !ok {
		return tool.Result{Success: false, Error: "module not found"}, fmt.Errorf("servicebus: module %q not registered", moduleID)
	}
	return m.Invoke(ctx, toolName, params)
}

var _ ports.ServiceBus = (*Bus)(nil)
