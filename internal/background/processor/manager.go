package processor

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/google/uuid"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
	"github.com/mbathepaul/digitorn/internal/background/store"
)

// TriggerID is the stable id for an (app, provider) pair, so re-arming the same
// trigger (e.g. on a config re-scan) upserts in place instead of duplicating.
func TriggerID(appID, provider string) string {
	sum := sha1.Sum([]byte(appID + "\x00" + provider))
	return "trg-" + hex.EncodeToString(sum[:8])
}

// Manager arms triggers, owns the shared durable intake (routing each delivery
// to the right trigger by provider name), starts every registered adapter, and
// mounts the HTTP-capable adapters' handlers. It is the boot-time assembly of
// the inbound side.
type Manager struct {
	store    *store.Store
	registry *adapter.Registry
	routes   map[string]route // provider name → armed trigger
}

type route struct{ appID, triggerID string }

// NewManager builds an empty manager over the store + adapter registry.
func NewManager(st *store.Store, reg *adapter.Registry) *Manager {
	return &Manager{store: st, registry: reg, routes: map[string]route{}}
}

// Arm persists a channel trigger (idempotent on id) and registers its
// provider→trigger route so inbound events from that provider land on the right
// activation.
func (m *Manager) Arm(ctx context.Context, spec TriggerSpec) (string, error) {
	return m.arm(ctx, spec, "channel")
}

// ArmSchedule arms a user-programmed session wake-up: same persist + route as a
// channel trigger, but marked Kind="schedule" so the ops surface lists/manages it
// separately. The spec's Activation binds the session to wake (Session/Owner).
func (m *Manager) ArmSchedule(ctx context.Context, spec TriggerSpec) (string, error) {
	return m.arm(ctx, spec, "schedule")
}

func (m *Manager) arm(ctx context.Context, spec TriggerSpec, kind string) (string, error) {
	cfg, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	tr := &store.Trigger{
		ID:         TriggerID(spec.AppID, spec.Provider),
		AppID:      spec.AppID,
		Provider:   spec.Provider,
		Adapter:    spec.Adapter,
		ConfigJSON: string(cfg),
		Enabled:    true,
		Kind:       kind,
	}
	if err := m.store.UpsertTrigger(ctx, tr); err != nil {
		return "", err
	}
	m.routes[spec.Provider] = route{appID: spec.AppID, triggerID: tr.ID}
	return tr.ID, nil
}

// Sink is the shared, durable, provider-routing intake. Every adapter pushes
// here BEFORE it ACKs its source (intake-before-process); redeliveries dedup.
func (m *Manager) Sink() adapter.Sink {
	return func(ctx context.Context, ev adapter.Event) error {
		rt, ok := m.routes[ev.Provider]
		if !ok {
			return fmt.Errorf("intake: no armed trigger for provider %q", ev.Provider)
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		dedup := ev.DedupKey
		if dedup == "" {
			dedup = uuid.NewString()
		}
		_, _, err = m.store.Enqueue(ctx, store.NewJob{
			AppID:     rt.appID,
			TriggerID: rt.triggerID,
			Provider:  ev.Provider,
			DedupKey:  ev.Provider + ":" + dedup,
			Payload:   payload,
		})
		return err
	}
}

// Start runs every registered adapter with the shared sink, then blocks until
// ctx is cancelled (adapters stop on the same ctx).
func (m *Manager) Start(ctx context.Context) error {
	sink := m.Sink()
	for _, ad := range m.registry.All() {
		go func(a adapter.Adapter) { _ = a.Start(ctx, sink) }(ad)
	}
	<-ctx.Done()
	return nil
}

// httpAdapter is the optional interface an adapter implements to contribute
// inbound HTTP routes (the webhook adapter).
type httpAdapter interface{ Handler() http.Handler }

// Handler returns the combined inbound HTTP handler: each HTTP-capable adapter
// is tried in turn, the first that doesn't 404 wins. Empty if none are HTTP.
func (m *Manager) Handler() http.Handler {
	var hs []http.Handler
	for _, ad := range m.registry.All() {
		if h, ok := ad.(httpAdapter); ok {
			hs = append(hs, h.Handler())
		}
	}
	if len(hs) == 0 {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range hs {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != http.StatusNotFound {
				for k, v := range rec.Header() {
					w.Header()[k] = v
				}
				w.WriteHeader(rec.Code)
				_, _ = w.Write(rec.Body.Bytes())
				return
			}
		}
		http.NotFound(w, r)
	})
}
