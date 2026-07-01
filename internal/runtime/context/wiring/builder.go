// Package wiring is the glue that turns CB-1 to CB-5 into a single
// runtime.ContextBuilder implementation the daemon plugs into the
// Engine at boot.
//
// Responsibilities :
//
//   - Build the per-agent ToolIndex (CB-1) from the app's modules +
//     the agent's restrictions, pre-filtered by the security gates
//     (SG-3 BuildAgentToolset).
//   - Run the adaptive injection planner (CB-2) to pick the mode
//     and assemble the LLM tool list.
//   - Assemble the system prompt with the 9 documented sections
//     (CB-4), in the doc order, with the user's system_prompt
//     LAST.
//
// The MetaDispatcher (CB-3) is wired separately on the Engine
// (Engine.Dispatcher) because it intercepts dispatch ; this package
// only owns the BuildFor side.
//
// CB-5 semantic search is opt-in : when an EmbeddingClient is
// provided, every per-agent index gets a SemanticIndex attached so
// the keyword scorer adds the hybrid contribution.
package wiring

import (
	"context"
	"sort"
	"sync"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/embeddings"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/context/injection"
	"github.com/digitornai/digitorn/internal/runtime/context/prompt"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

// AvailableActions returns every (module, action, spec) tuple the
// daemon knows about for a given app. Implemented outside this
// package by the daemon's module catalog. Each Builder receives one
// via the field below ; nil = empty universe (the agent sees no
// tools, only the always-direct primitives).
type AvailableActions interface {
	// ForApp returns the actions the given app's loaded modules
	// expose. The result is filtered later by SG-3 + CB-1 against
	// the per-agent capabilities ; this method just emits the raw
	// universe.
	ForApp(appID string) []policy.AvailableAction
}

// PromptContributors gathers the system-prompt contributions of the modules
// an agent is AUTHORIZED for. The concrete implementation (daemon side) wraps
// the module registry and calls each authorized module's PromptContributor
// methods. authorizedModules is the per-agent module set (the keys of the
// SG-3-filtered index categories), so contributions are authorization-gated
// at the source — an unauthorized module never contributes. nil source =
// no module-contributed sections (test/dev default).
type PromptContributors interface {
	Gather(scope domainmodule.PromptScope, authorizedModules []string) ([]domainmodule.PromptSection, map[string]string)
}

// Builder implements runtime.ContextBuilder by composing CB-1 (index),
// CB-2 (planner), CB-4 (prompt assembler) — and optionally CB-5
// (semantic search) when EmbeddingClient is set.
//
// Construct via New() — direct field initialisation works for
// tests but bypasses the safe defaults.
type Builder struct {
	// Contributors gathers module-contributed prompt sections + dynamic
	// tool prompts for the agent's authorized modules. nil = none.
	Contributors PromptContributors

	// Actions is the daemon's source of the per-app action
	// catalogue. Required ; if nil, the builder returns an empty
	// tool list (only the always-direct meta-tools + primitives
	// stay).
	Actions AvailableActions

	// IndexBuilder is the keyword + synonym builder (CB-1). Defaults
	// to index.NewBuilder() if nil.
	IndexBuilder *index.Builder

	// Planner is the injection-mode picker (CB-2). Defaults to
	// &injection.Planner{} if nil.
	Planner *injection.Planner

	// Assembler is the system-prompt builder (CB-4). Defaults to
	// prompt.NewAssembler() if nil.
	Assembler *prompt.Assembler

	// EmbeddingClient is optional. When non-nil, every per-agent
	// index gets a SemanticIndex attached for hybrid scoring. nil
	// = keyword-only (CB-1 behaviour).
	EmbeddingClient embeddings.EmbeddingClient

	// cache stores per (app_id + agent_id) BuildFor results so a
	// hot session doesn't rebuild the index every turn. The cache
	// is invalidated on app version change via the appVersion key.
	cache sync.Map // map[cacheKey]*cacheEntry
}

// New returns a Builder with default sub-components. Fluent setters
// (WithEmbeddings, WithActions, etc.) are provided for the daemon
// to fill the remaining fields before publishing.
func New(actions AvailableActions) *Builder {
	return &Builder{
		Actions:      actions,
		IndexBuilder: index.NewBuilder(),
		Planner:      &injection.Planner{},
		Assembler:    prompt.NewAssembler(),
	}
}

// WithEmbeddings enables CB-5 hybrid scoring. Returns the builder
// for chaining.
func (b *Builder) WithEmbeddings(client embeddings.EmbeddingClient) *Builder {
	b.EmbeddingClient = client
	return b
}

// WithContributors wires the module prompt-contribution source. Returns the
// builder for chaining.
func (b *Builder) WithContributors(c PromptContributors) *Builder {
	b.Contributors = c
	return b
}

// cacheKey identifies a per-agent build. App version is part of the
// key so deploying a new app version invalidates the cached index
// (the daemon's appmgr.RuntimeApp bumps a version string on every
// upgrade).
type cacheKey struct {
	AppID      string
	AppVersion string
	AgentID    string
}

type cacheEntry struct {
	once     sync.Once
	idx      *index.ToolIndex
	tools    []llm.ToolSpec
	mode     string
	sections []domainmodule.PromptSection
	dynamic  map[string]string
	err      error
}

// BuildFor implements runtime.ContextBuilder. The EXPENSIVE, session-
// independent artifacts (per-agent index, injected tool list, injection mode,
// module prompt sections) are built ONCE per (app, version, agent) and cached.
// The system prompt is RE-ASSEMBLED on every call from those artifacts plus the
// per-turn request — so the live memory snapshot is always current and a cached
// prompt can never leak one session's memory into another session of the same
// (app, agent). Only the cheap string assembly runs per turn ; the index +
// semantic build stays cached.
func (b *Builder) BuildFor(ctx context.Context, in runtime.ContextRequest) (runtime.ContextResult, error) {
	if b == nil {
		return runtime.ContextResult{}, nil
	}

	key := buildCacheKey(in)
	loaded, _ := b.cache.LoadOrStore(key, &cacheEntry{})
	entry := loaded.(*cacheEntry)
	entry.once.Do(func() {
		entry.idx, entry.tools, entry.mode, entry.sections, entry.dynamic, entry.err = b.buildArtifacts(ctx, in)
	})
	if entry.err != nil {
		return runtime.ContextResult{}, entry.err
	}
	return runtime.ContextResult{
		Tools:        entry.tools,
		SystemPrompt: b.assemblePrompt(in, entry),
		Mode:         entry.mode,
	}, nil
}

// assemblePrompt renders the system prompt fresh from the cached session-
// independent artifacts + the per-turn request. The memory snapshot is the
// only input that varies per turn / per session, so it is applied HERE and
// never cached — fixing both intra-session staleness and the cross-session
// leak of caching the whole prompt.
func (b *Builder) assemblePrompt(in runtime.ContextRequest, e *cacheEntry) string {
	asm := b.Assembler
	if asm == nil {
		asm = prompt.NewAssembler()
	}
	var siblings []schema.Agent
	if in.App != nil && in.App.Definition != nil {
		siblings = in.App.Definition.Agents
	}
	return asm.Assemble(prompt.PromptContext{
		Agent:              in.Agent,
		AppName:            in.AppName,
		AppVersion:         in.AppVersion,
		InjectionMode:      injection.Mode(e.mode),
		ToolIndex:          e.idx,
		InjectedTools:      e.tools,
		MemoryEnabled:      in.MemoryEnabled,
		Memory:             in.Memory,
		Specialists:        specialistsFor(in.Agent, siblings),
		ModuleSections:     e.sections,
		DynamicToolPrompts: e.dynamic,
	})
}

// buildArtifacts is the one-shot, session-independent work executed under
// sync.Once per cache key : the per-agent index, the injected tool list, the
// injection mode, and the module-contributed prompt sections + dynamic tool
// prompts. It does NOT assemble the prompt — that is per-turn (assemblePrompt),
// because the memory snapshot varies per turn and per session.
func (b *Builder) buildArtifacts(ctx context.Context, in runtime.ContextRequest) (*index.ToolIndex, []llm.ToolSpec, string, []domainmodule.PromptSection, map[string]string, error) {
	// 1. Resolve the action universe.
	var universe []policy.AvailableAction
	if b.Actions != nil && in.App != nil && in.App.Meta != nil {
		universe = b.Actions.ForApp(in.App.Meta.AppID)
	}

	// 2. Build the per-agent index (CB-1). The Builder applies the
	//    SG-3 filter so hidden / deny / over-risk / out-of-agent-set
	//    actions never land in the index.
	ib := b.IndexBuilder
	if ib == nil {
		ib = index.NewBuilder()
	}
	appActive := in.App != nil && in.App.Meta != nil && in.App.Meta.Enabled
	var caps *schema.CapabilitiesConfig
	if in.App != nil && in.App.Definition != nil && in.App.Definition.Tools != nil {
		caps = in.App.Definition.Tools.Capabilities
	}
	idx := ib.Build(appActive, caps, in.Agent, universe)

	// 3. Attach CB-5 semantic index when an EmbeddingClient is
	//    wired. Failure here is non-fatal : the keyword side still
	//    works ; we log via the next layer when surfaced.
	if b.EmbeddingClient != nil && idx != nil && len(idx.Tools) > 0 {
		semIdx, err := embeddings.NewSemanticIndex(ctx, b.EmbeddingClient,
			embeddings.BuildCorpus(idx.Tools))
		if err == nil && semIdx != nil {
			embeddings.Attach(idx, semIdx, b.EmbeddingClient)
		}
	}

	// 4. Pick the injection mode and build the tool list (CB-2).
	planner := b.Planner
	if planner == nil {
		planner = &injection.Planner{}
	}
	var rt *schema.RuntimeBlock
	if in.App != nil && in.App.Definition != nil {
		rt = in.App.Definition.Runtime
	}
	decision := planner.Plan(idx, in.Agent, rt)

	// 5. Build the EXACT native tool list the LLM receives this turn.
	// memory + agent_spawn tools are NOT universal context_builder builtins :
	// they belong to opt-in modules, so we append them only when the app
	// declared/loaded the owning module (the documented YAML contract). The
	// optional primitives (call_app / ask_user / use_skill) are appended only
	// when actually usable, so the model is never shown a tool it can't run.
	// Built BEFORE prompt assembly so the prompt describes EXACTLY these tools
	// (anti-pollution invariant).
	tools := decision.Tools
	if in.MemoryEnabled {
		tools = append(tools, injection.MemoryToolSpecs()...)
	}
	if in.AgentEnabled {
		tools = append(tools, injection.AgentToolSpec()...)
	}
	if in.CallAppEnabled {
		tools = append(tools, injection.CallAppSpec()...)
	}
	if in.AskUserEnabled {
		tools = append(tools, injection.AskUserSpec()...)
	}
	if in.UseSkillEnabled {
		tools = append(tools, injection.UseSkillSpec()...)
	}

	// 6. Gather module-contributed prompt sections + dynamic tool prompts —
	// faithful port of the reference daemon's get_prompt_sections() /
	// get_dynamic_tool_prompts(). Authorization-gated at the source : only
	// the agent's authorized modules (the SG-3-filtered index categories)
	// are asked to contribute, so an unauthorized module never reaches the
	// prompt.
	var moduleSections []domainmodule.PromptSection
	var dynamicToolPrompts map[string]string
	if b.Contributors != nil && idx != nil && len(idx.Categories) > 0 {
		authorized := make([]string, 0, len(idx.Categories))
		for m := range idx.Categories {
			authorized = append(authorized, m)
		}
		sort.Strings(authorized)
		scope := domainmodule.PromptScope{}
		if in.App != nil && in.App.Meta != nil {
			scope.AppID = in.App.Meta.AppID
		}
		if in.Agent != nil {
			scope.AgentID = in.Agent.ID
			scope.Role = in.Agent.Role
		}
		moduleSections, dynamicToolPrompts = b.Contributors.Gather(scope, authorized)
	}

	// The prompt is NOT assembled here — it is rendered per turn in
	// assemblePrompt from these cached artifacts + the live memory snapshot.
	return idx, tools, string(decision.Mode), moduleSections, dynamicToolPrompts, nil
}

// buildCacheKey extracts the cache key from a ContextRequest.
func buildCacheKey(in runtime.ContextRequest) cacheKey {
	k := cacheKey{}
	if in.App != nil && in.App.Meta != nil {
		k.AppID = in.App.Meta.AppID
	}
	if in.App != nil && in.App.Definition != nil {
		k.AppVersion = in.App.Definition.App.Version
	}
	if in.Agent != nil {
		k.AgentID = in.Agent.ID
	}
	return k
}

// IndexFor returns the per-agent ToolIndex previously built by
// BuildFor for the given (appID, agentID). The CB-3 MetaDispatcher
// uses this to resolve the index at dispatch time, satisfying the
// IndexLookup contract.
//
// Semantics :
//
//   - The cache is keyed by (AppID, AppVersion, AgentID). IndexFor
//     ignores the version : during a hot redeploy the engine's turn
//     ran BuildFor on the current version, so the live entry for
//     (appID, agentID) refers to that turn's version. If multiple
//     entries exist (the rare overlap window during redeploy), the
//     first matching entry whose build completed is returned —
//     stale-but-consistent beats wrong-version-mismatch.
//   - Returns nil when no entry exists yet (BuildFor was never
//     called for this pair) OR the entry's build errored. The
//     MetaDispatcher degrades gracefully on nil.
//   - Lock-free read : the underlying sync.Map handles concurrency.
func (b *Builder) IndexFor(appID, agentID string) *index.ToolIndex {
	if b == nil {
		return nil
	}
	var found *index.ToolIndex
	b.cache.Range(func(k, v any) bool {
		ck := k.(cacheKey)
		if ck.AppID != appID || ck.AgentID != agentID {
			return true
		}
		entry := v.(*cacheEntry)
		if entry.err != nil || entry.idx == nil {
			return true
		}
		found = entry.idx
		return false // stop on first hit
	})
	return found
}

// Invalidate drops the cache for a single (app_id, app_version,
// agent_id) tuple. Used when an app version is hot-redeployed.
// Empty fields invalidate every entry that matches the non-empty
// fields (e.g. invalidate every agent of an app by passing only
// appID).
func (b *Builder) Invalidate(appID, appVersion, agentID string) {
	if b == nil {
		return
	}
	b.cache.Range(func(k, _ any) bool {
		ck := k.(cacheKey)
		if appID != "" && ck.AppID != appID {
			return true
		}
		if appVersion != "" && ck.AppVersion != appVersion {
			return true
		}
		if agentID != "" && ck.AgentID != agentID {
			return true
		}
		b.cache.Delete(k)
		return true
	})
}

// specialistsFor resolves a coordinator's delegate_to targets to their
// id + specialty, for the agent-pool prompt section. Returns nil for
// non-coordinators or agents with no delegation targets.
func specialistsFor(agent *schema.Agent, siblings []schema.Agent) []prompt.SpecialistEntry {
	if agent == nil || agent.Role != "coordinator" || len(agent.DelegateTo) == 0 {
		return nil
	}
	specialty := make(map[string]string, len(siblings))
	for i := range siblings {
		specialty[siblings[i].ID] = siblings[i].Specialty
	}
	out := make([]prompt.SpecialistEntry, 0, len(agent.DelegateTo))
	for _, id := range agent.DelegateTo {
		out = append(out, prompt.SpecialistEntry{ID: id, Specialty: specialty[id]})
	}
	return out
}
