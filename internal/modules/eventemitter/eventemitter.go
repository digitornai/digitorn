// Package eventemitter provides helpers for modules to emit events on the EventBus.
// It wraps the EventBus and context extraction, making it easy for any module
// to emit events without worrying about the details.
package eventemitter

import (
	"context"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/ports"
)

// Emit is a helper function that emits an event on the EventBus if available.
// It extracts the identity from context and adds standard metadata.
// Non-blocking and best-effort: never affects the caller.
//
// Usage:
//
//	func (m *Module) someAction(ctx context.Context) {
//	    // ... do work ...
//	    eventemitter.Emit(ctx, "module.event.type", map[string]any{
//	        "key": "value",
//	    })
//	}
func Emit(ctx context.Context, eventType string, data map[string]any) {
	busRaw, ok := tool.EventBusFromContext(ctx)
	if !ok || busRaw == nil {
		return
	}
	bus, ok := busRaw.(ports.EventBus)
	if !ok || bus == nil {
		return
	}

	id, _ := tool.IdentityFromContext(ctx)
	evt := ports.Event{
		Topic:  eventType,
		Type:   eventType,
		Source: id.AppID,
		Data:   data,
		Metadata: map[string]any{
			"session_id": id.SessionID,
			"user_id":    id.UserID,
			"agent_id":   id.AgentID,
			"timestamp":  time.Now().UTC().Format(time.RFC3339),
		},
	}
	_ = bus.Publish(ctx, evt)
}

// EmitWithModule is like Emit but adds a module_id to metadata.
func EmitWithModule(ctx context.Context, moduleID, eventType string, data map[string]any) {
	busRaw, ok := tool.EventBusFromContext(ctx)
	if !ok || busRaw == nil {
		return
	}
	bus, ok := busRaw.(ports.EventBus)
	if !ok || bus == nil {
		return
	}

	id, _ := tool.IdentityFromContext(ctx)
	evt := ports.Event{
		Topic:  eventType,
		Type:   eventType,
		Source: id.AppID,
		Data:   data,
		Metadata: map[string]any{
			"session_id": id.SessionID,
			"user_id":    id.UserID,
			"agent_id":   id.AgentID,
			"module_id":  moduleID,
			"timestamp":  time.Now().UTC().Format(time.RFC3339),
		},
	}
	_ = bus.Publish(ctx, evt)
}
