package ports

import (
	"context"

	"github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
)

// ServiceBus is the in-process registry of running modules. It enables
// inter-module action calls without direct compile-time coupling.
type ServiceBus interface {
	// Register registers a running module. Idempotent: re-registering with
	// the same ID replaces the previous binding.
	Register(m module.Module) error

	// Unregister removes a module by ID.
	Unregister(id string) error

	// Get returns the module with the given ID, or nil if absent.
	Get(id string) (module.Module, bool)

	// List returns all registered modules.
	List() []module.Module

	// Call dispatches a tool call on the named module.
	Call(ctx context.Context, moduleID, toolName string, params []byte) (tool.Result, error)
}
