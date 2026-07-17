package eventemitter

import (
	"context"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/ports"
)

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
