package module

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"

	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
)

type Factory func() domainmodule.Module

type Bus interface {
	Register(m domainmodule.Module) error
	Unregister(id string) error
}

type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
	manifests map[string]domainmodule.Manifest
	instances map[string]domainmodule.Module
	configs   map[string]map[string]any
	states    map[string]State
	failed    map[string]error
	bus       Bus
}

func NewRegistry() *Registry {
	return &Registry{
		factories: map[string]Factory{},
		manifests: map[string]domainmodule.Manifest{},
		instances: map[string]domainmodule.Module{},
		configs:   map[string]map[string]any{},
		states:    map[string]State{},
		failed:    map[string]error{},
	}
}

func (r *Registry) WithBus(b Bus) *Registry {
	r.mu.Lock()
	r.bus = b
	r.mu.Unlock()
	return r
}

func (r *Registry) Register(f Factory) error {
	if f == nil {
		return fmt.Errorf("registry: nil factory")
	}
	probe := f()
	if probe == nil {
		return fmt.Errorf("registry: factory returned nil module")
	}
	m := probe.Manifest()
	if m.ID == "" {
		return fmt.Errorf("registry: module manifest has empty ID")
	}
	if !platformAllowed(m.SupportedPlatforms) {
		return nil
	}
	r.mu.Lock()
	r.factories[m.ID] = f
	r.manifests[m.ID] = m
	r.states[m.ID] = StateLoaded
	r.mu.Unlock()
	return nil
}

func (r *Registry) MustRegister(f Factory) {
	if err := r.Register(f); err != nil {
		panic(err)
	}
}

func (r *Registry) Configure(id string, cfg map[string]any) {
	r.mu.Lock()
	r.configs[id] = cfg
	r.mu.Unlock()
}

func (r *Registry) Get(id string) (domainmodule.Module, error) {
	r.mu.RLock()
	if m, ok := r.instances[id]; ok {
		r.mu.RUnlock()
		return m, nil
	}
	if err, ok := r.failed[id]; ok {
		r.mu.RUnlock()
		return nil, err
	}
	f, ok := r.factories[id]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("registry: unknown module %q", id)
	}
	m := f()
	if m == nil {
		err := fmt.Errorf("registry: factory for %q returned nil", id)
		r.markFailed(id, err)
		return nil, err
	}
	r.mu.Lock()
	r.instances[id] = m
	r.mu.Unlock()
	return m, nil
}

func (r *Registry) Create(id string) (domainmodule.Module, error) {
	r.mu.RLock()
	f, ok := r.factories[id]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("registry: unknown module %q", id)
	}
	return f(), nil
}

func (r *Registry) Has(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[id]
	return ok
}

func (r *Registry) IDs() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.factories))
	for id := range r.factories {
		out = append(out, id)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

func (r *Registry) Manifests() []domainmodule.Manifest {
	r.mu.RLock()
	out := make([]domainmodule.Manifest, 0, len(r.manifests))
	for _, m := range r.manifests {
		out = append(out, m)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) Manifest(id string) (domainmodule.Manifest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.manifests[id]
	return m, ok
}

func (r *Registry) State(id string) State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.states[id]
}

func (r *Registry) Start(ctx context.Context, id string) error {
	m, err := r.Get(id)
	if err != nil {
		return err
	}
	r.setState(id, StateStarting)
	cfg := r.configFor(id)
	if err := m.Init(ctx, cfg); err != nil {
		r.markFailed(id, err)
		return fmt.Errorf("init %s: %w", id, err)
	}
	if err := m.Start(ctx); err != nil {
		r.markFailed(id, err)
		return fmt.Errorf("start %s: %w", id, err)
	}
	r.setState(id, StateActive)
	if bus := r.snapshotBus(); bus != nil {
		if err := bus.Register(m); err != nil {
			r.markFailed(id, err)
			return fmt.Errorf("bus register %s: %w", id, err)
		}
	}
	return nil
}

func (r *Registry) StartAll(ctx context.Context) []error {
	var errs []error
	for _, id := range r.IDs() {
		if err := r.Start(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (r *Registry) StartExcept(ctx context.Context, exclude []string) []error {
	excl := make(map[string]struct{}, len(exclude))
	for _, id := range exclude {
		excl[id] = struct{}{}
	}
	var errs []error
	for _, id := range r.IDs() {
		if _, skip := excl[id]; skip {
			continue
		}
		if err := r.Start(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (r *Registry) Pause(ctx context.Context, id string) error {
	r.mu.RLock()
	m, ok := r.instances[id]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	p, ok := m.(domainmodule.Pauser)
	if !ok {
		return nil
	}
	if err := p.Pause(ctx); err != nil {
		return fmt.Errorf("pause %s: %w", id, err)
	}
	r.setState(id, StatePaused)
	return nil
}

func (r *Registry) Resume(ctx context.Context, id string) error {
	r.mu.RLock()
	m, ok := r.instances[id]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	p, ok := m.(domainmodule.Pauser)
	if !ok {
		return nil
	}
	if err := p.Resume(ctx); err != nil {
		return fmt.Errorf("resume %s: %w", id, err)
	}
	r.setState(id, StateActive)
	return nil
}

func (r *Registry) UpdateConfig(ctx context.Context, id string, cfg map[string]any) error {
	r.mu.Lock()
	r.configs[id] = cfg
	r.mu.Unlock()
	r.mu.RLock()
	m, ok := r.instances[id]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	rl, ok := m.(domainmodule.Reloader)
	if !ok {
		return nil
	}
	if err := rl.UpdateConfig(ctx, cfg); err != nil {
		return fmt.Errorf("update_config %s: %w", id, err)
	}
	return nil
}

func (r *Registry) Stop(ctx context.Context, id string) error {
	r.mu.RLock()
	m, ok := r.instances[id]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	r.setState(id, StateStopping)
	if err := m.Stop(ctx); err != nil {
		r.markFailed(id, err)
		return fmt.Errorf("stop %s: %w", id, err)
	}
	if bus := r.snapshotBus(); bus != nil {
		_ = bus.Unregister(id)
	}
	r.setState(id, StateDisabled)
	return nil
}

func (r *Registry) StopAll(ctx context.Context) []error {
	r.mu.RLock()
	ids := make([]string, 0, len(r.instances))
	for id := range r.instances {
		ids = append(ids, id)
	}
	r.mu.RUnlock()
	var errs []error
	for _, id := range ids {
		if err := r.Stop(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (r *Registry) markFailed(id string, err error) {
	r.mu.Lock()
	r.failed[id] = err
	r.states[id] = StateError
	delete(r.instances, id)
	r.mu.Unlock()
}

func (r *Registry) setState(id string, s State) {
	r.mu.Lock()
	r.states[id] = s
	r.mu.Unlock()
}

func (r *Registry) configFor(id string) map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.configs[id]
}

func (r *Registry) snapshotBus() Bus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bus
}

var Default = NewRegistry()

func Register(f Factory) error { return Default.Register(f) }
func MustRegister(f Factory)   { Default.MustRegister(f) }

func platformAllowed(platforms []domainmodule.Platform) bool {
	if len(platforms) == 0 {
		return true
	}
	cur := domainmodule.Platform(runtime.GOOS)
	for _, p := range platforms {
		if p == cur || p == "all" || p == "" {
			return true
		}
	}
	return false
}
