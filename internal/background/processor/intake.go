package processor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/digitornai/digitorn/internal/background/adapter"
	"github.com/digitornai/digitorn/internal/background/store"
)

// Intake is the durable Sink: it records every inbound Event as a pending job
// BEFORE the adapter ACKs its source. Idempotent by DedupKey — a redelivery is
// dropped, never enqueued twice. This is the "intake-before-process" guarantee.
type Intake struct {
	store    *store.Store
	provider string
	appID    string
	trigger  string
}

// NewIntake builds a sink bound to one armed trigger (app + provider + trigger
// id), so each persisted job knows which activation config to apply.
func NewIntake(st *store.Store, appID, provider, triggerID string) *Intake {
	return &Intake{store: st, provider: provider, appID: appID, trigger: triggerID}
}

// Sink returns the adapter.Sink closure.
func (i *Intake) Sink() adapter.Sink {
	return func(ctx context.Context, ev adapter.Event) error {
		payload, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("intake encode: %w", err)
		}
		dedup := ev.DedupKey
		if dedup == "" {
			dedup = i.provider + ":" + uuid.NewString() // no natural key → unique per delivery
		} else {
			dedup = i.provider + ":" + dedup // namespace per provider
		}
		_, _, err = i.store.Enqueue(ctx, store.NewJob{
			AppID:     i.appID,
			TriggerID: i.trigger,
			Provider:  i.provider,
			DedupKey:  dedup,
			Payload:   payload,
		})
		return err
	}
}
