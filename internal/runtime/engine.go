package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	coremw "github.com/digitornai/digitorn/internal/core/middleware"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/ports"
	"github.com/digitornai/digitorn/internal/runtime/adapter"
	"github.com/digitornai/digitorn/internal/runtime/behavior"
	"github.com/digitornai/digitorn/internal/runtime/context/prompt"
	"github.com/digitornai/digitorn/internal/runtime/contextcompact"
	"github.com/digitornai/digitorn/internal/runtime/contextsvc"
	"github.com/digitornai/digitorn/internal/runtime/docextract"
	"github.com/digitornai/digitorn/internal/runtime/errclass"
	"github.com/digitornai/digitorn/internal/runtime/flow"
	"github.com/digitornai/digitorn/internal/runtime/hooks"
	"github.com/digitornai/digitorn/internal/runtime/policy"
	"github.com/digitornai/digitorn/internal/runtime/policy/approval"
	"github.com/digitornai/digitorn/internal/runtime/projectsettings"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/runtime/toolname"
	"github.com/digitornai/digitorn/internal/runtime/turn"
	"github.com/digitornai/digitorn/internal/runtime/workdir"
	"github.com/digitornai/digitorn/internal/safego"
)

type PathPolicySource interface {
	PathPolicyFor(appID, sessionID string) (workdir.PathPolicy, bool)
}

type AppLookup interface {
	Get(ctx context.Context, appID string) (*appmgr.RuntimeApp, error)
}

type SessionAccess interface {
	State(sid string) (*sessionstore.SessionState, error)
	AppendDurable(ctx context.Context, ev sessionstore.Event) (uint64, error)
	Append(ctx context.Context, ev sessionstore.Event) (uint64, error)
}

type LLMChat interface {
	Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error)
}

type LLMStream interface {
	ChatStream(ctx context.Context, req *llm.ChatRequest) (<-chan *llm.ChatChunk, error)
}

type BlobLoader interface {
	LoadBlob(ctx context.Context, hash string) ([]byte, error)
}

type Runner interface {
	Run(ctx context.Context, in TurnInput) (*TurnResult, error)
}

type EmergencyCompactor interface {
	CompactSession(ctx context.Context, sessionID, strategy string, keepLast int) error
}

// CredentialResolver returns a user's stored provider key (apiKey + optional
// baseURL) for BYOK injection. Implemented by *credentials.Resolver as an O(1)
// in-memory lookup; ok=false when the user has none (caller falls back). nil =
// per-user injection disabled.
type CredentialResolver interface {
	Lookup(ctx context.Context, userID, provider string) (apiKey, baseURL string, ok bool)
}

// AppSecretLookup resolves installation-scoped {{secret.X}} placeholders for an
// app at runtime (flow URLs, module headers). nil = env fallback only.
type AppSecretLookup interface {
	Get(appID, key string) (string, bool)
}

type Engine struct {
	Apps         AppLookup
	Sessions     SessionAccess
	LLM          LLMChat
	Blobs        BlobLoader
	CredResolver CredentialResolver // per-user BYOK key injection; nil = disabled
	AppSecrets   AppSecretLookup    // per-app secret vault; nil = env fallback only
	// MediaGen calls the gateway's dedicated image/video generation endpoints
	// for agents whose brain kind is "image"/"video" (these are NOT served by
	// chat-completions). nil = media generation disabled.
	MediaGen   MediaGenerator
	Tools      ToolCatalog
	Dispatcher ToolDispatcher

	allowedSigs       sync.Map
	SkillLoader       SkillLoader
	ModelWindowLookup func(model string) int

	Compactor          EmergencyCompactor
	ContextTouch       func(sessionID string)
	ContextIncrement   func(sessionID string, deltaTokens int)
	ContextRecordParts func(sessionID string, system, tools []string)
	PrepareSummary     func(sessionID, jwt string)
	MicroCompactView   bool
	ContextLookup      func(sessionID string) (contextsvc.ContextView, bool)
	ContextRecordRatio func(sessionID string, ratio float64)
	Pool               *turn.Pool

	taskSeq sync.Map

	SubAgentPool *turn.Pool

	LLMSem *PrioritySemaphore

	IDGen  turn.IDGen
	Logger *slog.Logger

	PolicyEvaluator PolicyEvaluator

	PathPolicies PathPolicySource

	ApprovalRegistry *approval.Registry

	Context ContextBuilder

	Hooks HookSource

	MaxToolIterations int

	ToolTimeout time.Duration

	BackgroundNotifications BackgroundNotifier

	Streaming bool

	ResponseNormalizer func(*llm.ChatResponse)

	behaviorMu      sync.Mutex
	behaviorEngines map[string]*behavior.Engine

	MiddlewareRetriever coremw.Retriever

	MiddlewareCustomFactory func(name string, cfg map[string]any) (ports.AppMiddleware, error)

	middlewareMu    sync.Mutex
	middlewarePipes map[string]*coremw.Pipeline
}

func (e *Engine) middlewareFor(app *appmgr.RuntimeApp) *coremw.Pipeline {
	if app == nil || app.Definition == nil || app.Definition.Runtime == nil ||
		len(app.Definition.Runtime.Middleware) == 0 {
		return nil
	}
	appID := ""
	if app.Meta != nil {
		appID = app.Meta.AppID
	}
	e.middlewareMu.Lock()
	defer e.middlewareMu.Unlock()
	if e.middlewarePipes == nil {
		e.middlewarePipes = map[string]*coremw.Pipeline{}
	}
	if p, ok := e.middlewarePipes[appID]; ok {
		return p
	}
	p := coremw.Build(app.Definition.Runtime.Middleware, coremw.Deps{
		Retriever:     e.MiddlewareRetriever,
		CustomFactory: e.MiddlewareCustomFactory,
	}, e.Logger)
	e.middlewarePipes[appID] = p
	return p
}

func (e *Engine) behaviorFor(app *appmgr.RuntimeApp) *behavior.Engine {
	if app == nil || app.Definition == nil || app.Definition.Security == nil ||
		app.Definition.Security.Behavior == nil {
		return nil
	}
	appID := ""
	if app.Meta != nil {
		appID = app.Meta.AppID
	}
	e.behaviorMu.Lock()
	defer e.behaviorMu.Unlock()
	if e.behaviorEngines == nil {
		e.behaviorEngines = map[string]*behavior.Engine{}
	}
	if be, ok := e.behaviorEngines[appID]; ok {
		return be
	}
	be := behavior.New(app.Definition.Security.Behavior)
	e.behaviorEngines[appID] = be
	return be
}

func (e *Engine) CleanupBehaviorSession(appID, sid string) {
	e.behaviorMu.Lock()
	be := e.behaviorEngines[appID]
	e.behaviorMu.Unlock()
	if be != nil {
		be.CleanupSession(sid)
	}
}

const defaultMaxToolIterations = 100

const defaultMaxStopVetoes = 2

func resolveMaxStopVetoes(rt *schema.RuntimeBlock) int {
	if rt != nil && rt.MaxStopRetries != nil {
		if v := *rt.MaxStopRetries; v >= 0 {
			return v
		}
	}
	return defaultMaxStopVetoes
}

func toolSignature(module, action string, args map[string]any) string {
	base := module + "." + action
	primaryKeys := []string{"command", "path", "query", "url", "name"}
	for _, k := range primaryKeys {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return base + ":" + s
			}
		}
	}
	return base
}

func (e *Engine) loadAllowedSigs(sessionID string, snap sessionstore.SessionSnapshot) {
	if len(snap.AllowedSignatures) == 0 {
		return
	}
	set := make(map[string]struct{}, len(snap.AllowedSignatures))
	for _, sig := range snap.AllowedSignatures {
		set[sig] = struct{}{}
	}
	e.allowedSigs.Store(sessionID, set)
}

func (e *Engine) isToolAllowed(sessionID, module, action string, args map[string]any) bool {
	v, ok := e.allowedSigs.Load(sessionID)
	if !ok {
		return false
	}
	set := v.(map[string]struct{})
	sig := toolSignature(module, action, args)
	if _, ok := set[sig]; ok {
		return true
	}
	base := module + "." + action
	_, ok = set[base]
	return ok
}

func (e *Engine) addAllowedSig(sessionID, sig string) {
	v, _ := e.allowedSigs.LoadOrStore(sessionID, map[string]struct{}{})
	set := v.(map[string]struct{})
	set[sig] = struct{}{}
	e.allowedSigs.Store(sessionID, set)
}

func loadProjectCaps(workdir string) *schema.CapabilitiesConfig {
	s, err := projectsettings.Load(workdir)
	if err != nil || s == nil {
		return nil
	}
	return s.Capabilities()
}

func (e *Engine) loadBlob(ctx context.Context, hash string) ([]byte, error) {
	if e.Blobs == nil {
		return nil, nil
	}
	return e.Blobs.LoadBlob(ctx, hash)
}

func (e *Engine) tools() ToolCatalog {
	if e.Tools == nil {
		return NoToolsCatalog{}
	}
	return e.Tools
}

// blobPutter is the Put-capable subset of the blob store. e.Blobs is wired to
// the concrete *blobstore.Store (which has Put) but typed as BlobLoader (Get
// only), so we assert to this at runtime to persist generated media bytes.
type blobPutter interface {
	Put(ctx context.Context, mime string, r io.Reader) (sessionstore.BlobRef, error)
}

func (e *Engine) buildAssistantParts(ctx context.Context, resp *llm.ChatResponse) []sessionstore.MessagePart {
	if resp == nil {
		return nil
	}
	var parts []sessionstore.MessagePart
	if resp.Content != "" {
		parts = append(parts, sessionstore.MessagePart{
			Type: sessionstore.PartTypeText,
			Text: resp.Content,
		})
	}
	for _, tc := range resp.ToolCalls {
		parts = append(parts, sessionstore.MessagePart{
			Type: sessionstore.PartTypeToolCall,
			ToolCall: &sessionstore.ToolCallSpec{
				ID:   tc.ID,
				Name: tc.Name,
				Args: tc.Arguments,
			},
		})
	}
	// Natively-generated media (image/video). URL-backed parts (e.g. a hosted
	// video) ride as-is ; inline bytes are persisted to the content-addressed
	// blob store and referenced by hash — mirrors dispatch/busadapter for tool
	// outputs, keeping the durable event log free of megabytes of base64.
	for i := range resp.OutputMedia {
		mp := resp.OutputMedia[i]
		pt := sessionstore.PartTypeImage
		if mp.Type == llm.ContentTypeVideo {
			pt = sessionstore.PartTypeVideo
		}
		if mp.URL != "" {
			parts = append(parts, sessionstore.MessagePart{
				Type:      pt,
				URL:       mp.URL,
				Thumbnail: mp.Thumbnail,
				Blob:      &sessionstore.BlobRef{Mime: mp.Mime},
			})
			continue
		}
		if len(mp.Data) == 0 {
			continue
		}
		putter, ok := e.Blobs.(blobPutter)
		if !ok {
			continue
		}
		ref, err := putter.Put(ctx, mp.Mime, bytes.NewReader(mp.Data))
		if err != nil {
			if e.Logger != nil {
				e.Logger.Warn("buildAssistantParts: blob put failed", slog.String("err", err.Error()))
			}
			continue
		}
		parts = append(parts, sessionstore.MessagePart{
			Type:      pt,
			Blob:      &ref,
			Thumbnail: mp.Thumbnail,
		})
	}
	return parts
}

// MediaGenerator calls the gateway's image/video generation endpoints. The
// concrete impl lives in internal/runtime/mediagen.
type MediaGenerator interface {
	GenerateImage(ctx context.Context, model, prompt, bearer string) ([]llm.ContentPart, error)
	GenerateVideo(ctx context.Context, model, prompt, bearer string) ([]llm.ContentPart, error)
}

// generateMedia handles an image/video agent turn: it generates media from the
// last user prompt via the gateway's dedicated endpoints and returns it as a
// terminal assistant response (no tools, no iteration). The persist + turn-end
// flow then stores the media parts exactly like any other assistant message.
func (e *Engine) generateMedia(ctx context.Context, in TurnInput, agent *schema.Agent, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	if e.MediaGen == nil {
		return nil, fmt.Errorf("media generation not configured")
	}
	prompt := lastUserText(req.Messages)
	if prompt == "" {
		return nil, fmt.Errorf("media generation: no prompt")
	}
	var media []llm.ContentPart
	var err error
	switch agent.Brain.Kind {
	case "image":
		media, err = e.MediaGen.GenerateImage(ctx, agent.Brain.Model, prompt, in.UserJWT)
	case "video":
		media, err = e.MediaGen.GenerateVideo(ctx, agent.Brain.Model, prompt, in.UserJWT)
	default:
		return nil, fmt.Errorf("media generation: unsupported kind %q", agent.Brain.Kind)
	}
	if err != nil {
		return nil, err
	}
	return &llm.ChatResponse{
		Model:        agent.Brain.Model,
		FinishReason: "stop",
		OutputMedia:  media,
	}, nil
}

// lastUserText returns the text of the most recent user message — the prompt
// fed to image/video generation.
func lastUserText(msgs []llm.ChatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		if t := strings.TrimSpace(msgs[i].Content); t != "" {
			return t
		}
		var b strings.Builder
		for _, p := range msgs[i].Parts {
			if p.Type == llm.ContentTypeText && p.Text != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(p.Text)
			}
		}
		if t := strings.TrimSpace(b.String()); t != "" {
			return t
		}
	}
	return ""
}

type slogReporter struct {
	log *slog.Logger
}

func (r *slogReporter) Warn(msg string, kv ...any) {
	if r == nil || r.log == nil {
		return
	}
	r.log.Warn(msg, kv...)
}

var _ Runner = (*Engine)(nil)

func New(apps AppLookup, sessions SessionAccess, llmClient LLMChat, logger *slog.Logger) (*Engine, error) {
	if apps == nil {
		return nil, errors.New("runtime: nil AppLookup")
	}
	if sessions == nil {
		return nil, errors.New("runtime: nil SessionAccess")
	}
	if llmClient == nil {
		return nil, errors.New("runtime: nil LLMChat")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		Apps:         apps,
		Sessions:     sessions,
		LLM:          llmClient,
		Pool:         turn.NewPool(turn.PoolConfig{}),
		SubAgentPool: turn.NewPool(turn.PoolConfig{}),
		IDGen:        uuid.NewString,
		Logger:       logger,
	}, nil
}

type TurnInput struct {
	AppID     string
	SessionID string
	UserJWT   string
	UserID    string
	Mode      string

	Skill string

	Template string

	AgentID string

	AgentRunID string

	SubAgent bool
}

// injectTemplateDirective injects the selected template's system_prompt as a
// durable system directive (same path as skills). Resolved by id from the app
// YAML; persisted, so it stays active for the session.
func (e *Engine) injectTemplateDirective(ctx context.Context, in TurnInput, correlationID string, app *appmgr.RuntimeApp, snap *sessionstore.SessionSnapshot) {
	if in.Template == "" || app == nil || app.Definition == nil {
		return
	}
	var tpl *schema.TemplateBlock
	for i := range app.Definition.Templates {
		if app.Definition.Templates[i].ID == in.Template {
			tpl = &app.Definition.Templates[i]
			break
		}
	}
	if tpl == nil {
		e.Logger.Warn("runtime: template not found",
			slog.String("template", in.Template), slog.String("app", in.AppID))
		return
	}
	e.seedTemplateWorkdir(in, app, tpl)
	if strings.TrimSpace(tpl.SystemPrompt) == "" {
		return
	}
	body := "Template " + tpl.Name + " — follow these instructions:\n\n" + tpl.SystemPrompt
	content := wrapRuntimeDirective("template", "high", body)
	e.injectSystemDirective(ctx, in, correlationID, content, DirectiveTemplate,
		map[string]any{"template_id": tpl.ID, "name": tpl.Name}, nil)
	if st, err := e.Sessions.State(in.SessionID); err == nil && st != nil {
		*snap = st.Snapshot()
	}
}

// seedTemplateWorkdir copies a template's seed_dir into the session workdir,
// but only when that workdir is still empty so a live project is never touched.
func (e *Engine) seedTemplateWorkdir(in TurnInput, app *appmgr.RuntimeApp, tpl *schema.TemplateBlock) {
	if tpl.SeedDir == "" || app.BundleDir == "" || e.PathPolicies == nil {
		return
	}
	pp, ok := e.PathPolicies.PathPolicyFor(in.AppID, in.SessionID)
	if !ok || !pp.HasWorkdir() {
		return
	}
	dst := pp.Root()
	// Seed only when the workdir holds no real project files yet. Ignore
	// hidden meta the daemon creates at session start (.digitorn shadow
	// repo, .git) so the seed still lands in an otherwise-fresh workdir.
	if entries, err := os.ReadDir(dst); err == nil {
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				return
			}
		}
	}
	base := filepath.Clean(app.BundleDir)
	src := filepath.Clean(filepath.Join(base, filepath.FromSlash(tpl.SeedDir)))
	if src != base && !strings.HasPrefix(src, base+string(filepath.Separator)) {
		return
	}
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return
	}
	if err := copyTreeNoClobber(src, dst); err != nil {
		e.Logger.Warn("runtime: template seed failed",
			slog.String("template", tpl.ID), slog.String("err", err.Error()))
		return
	}
	e.Logger.Info("runtime: template workdir seeded",
		slog.String("template", tpl.ID), slog.String("workdir", dst))
}

func copyTreeNoClobber(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if _, err := os.Stat(target); err == nil {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

type TurnResult struct {
	Seq     uint64
	Content string
	TurnID  string
}

var errModeTimeout = errors.New("turn mode timeout exceeded")

func (e *Engine) Run(ctx context.Context, in TurnInput) (*TurnResult, error) {
	if in.AppID == "" || in.SessionID == "" {
		return nil, errors.New("runtime: AppID and SessionID required")
	}

	ctx = llm.WithUserJWT(ctx, in.UserJWT)

	app, err := e.Apps.Get(ctx, in.AppID)
	if err != nil {
		return nil, fmt.Errorf("runtime: lookup app %q: %w", in.AppID, err)
	}
	if app == nil || app.Definition == nil {
		return nil, fmt.Errorf("runtime: app %q has no Definition", in.AppID)
	}
	if app.Definition.Flow != nil && !in.SubAgent {
		return e.runFlow(ctx, app, in)
	}

	if len(app.Definition.Agents) == 0 {
		return nil, fmt.Errorf("runtime: app %q has no agents", in.AppID)
	}
	agent := resolveAgent(app.Definition, in.AgentID)
	if agent == nil {
		return nil, fmt.Errorf("runtime: app %q has no agent %q", in.AppID, in.AgentID)
	}

	state, err := e.Sessions.State(in.SessionID)
	if err != nil {
		return nil, fmt.Errorf("runtime: load session %q: %w", in.SessionID, err)
	}
	if state == nil {
		return nil, fmt.Errorf("runtime: session %q has no state", in.SessionID)
	}
	preSnap := state.Snapshot()

	// Drop the user's message attachments into <workdir>/attachments/ so the
	// agent can open them with `read` (see biSession for the context hint).
	e.materializeAttachments(ctx, preSnap)

	agent = applyEntryAgent(app.Definition, agent, in.AgentID, preSnap.EntryAgent)

	if ovr := e.modelOverrideFor(in.SessionID, agent.ID, preSnap.ModelOverrides); ovr != "" {
		ag := *agent
		ag.Brain.Model = ovr
		// Cross-provider pin: the model belongs to a different provider than the
		// brain (BYOK, e.g. a local LM Studio model). Switch the provider too so
		// the BYOK resolution below looks up THAT credential (base_url + key) from
		// the vault instead of the brain's original provider. The embedded auth is
		// then overridden by the resolved credential.
		if pov := e.providerOverrideFor(in.SessionID, agent.ID, preSnap.ProviderOverrides); pov != "" {
			ag.Brain.Provider = pov
		}
		// Per-model max generation override (BYOK models whose limits the gateway
		// doesn't know) → override the brain's max_tokens for this session.
		if out := e.outputOverrideFor(in.SessionID, agent.ID, preSnap.OutputTokenOverrides); out > 0 {
			v := out
			ag.Brain.MaxTokens = &v
		}
		if ag.Brain.Context != nil {
			cc := *ag.Brain.Context
			cc.MaxTokens = 0
			ag.Brain.Context = &cc
		}
		agent = &ag
	}

	if _, err := turn.RecoverInFlight(ctx, preSnap, e.Sessions); err != nil {
		return nil, fmt.Errorf("runtime: recover in-flight: %w", err)
	}

	pool := e.Pool
	if in.SubAgent && e.SubAgentPool != nil {
		pool = e.SubAgentPool
	}
	tr, err := turn.New(turn.Options{
		SessionID: in.SessionID,
		AppID:     in.AppID,
		AgentID:   agent.ID,
		UserID:    in.UserID,
		UserJWT:   in.UserJWT,
		Pool:      pool,
		Sink:      e.Sessions,
		IDGen:     e.IDGen,
		Logger:    e.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: new turn: %w", err)
	}
	if err := tr.Start(ctx); err != nil {
		return nil, fmt.Errorf("runtime: start turn: %w", err)
	}

	if preSnap.TurnCount <= 1 {
		ssRes := e.fireHook(ctx, in.AppID, agent, schema.HookEventSessionStart, hooks.Payload{
			AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID, TurnID: tr.ID,
		})
		e.applyInjections(ctx, in, tr, ssRes.Injects)
	}

	turnStartPayload := withTurnState(hooks.Payload{
		AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID, TurnID: tr.ID,
	}, e.computeHookMetrics(state.Snapshot(), agent, "", 0))
	turnStartRes := e.fireHook(ctx, in.AppID, agent, schema.HookEventTurnStart, turnStartPayload)
	e.applyInjections(ctx, in, tr, turnStartRes.Injects)

	e.injectBackgroundNotifications(ctx, in, tr.ID)

	snap := state.Snapshot()
	e.loadAllowedSigs(in.SessionID, snap)

	res, endMetrics, runErr := e.runPhases(ctx, tr, app, agent, snap, in)
	if runErr != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if isCancellation(ctx, runErr) {
			reason := runErr.Error()
			if cause := context.Cause(ctx); cause != nil && cause != ctx.Err() {
				reason = cause.Error()
			} else if cerr := ctx.Err(); cerr != nil {
				reason = cerr.Error()
			}
			if errors.Is(context.Cause(ctx), ErrTurnSafetyCutoff) {
				e.persistInterruptedAssistant(tr, in,
					"[Turn stopped: no progress for the safety window — the task may be stuck (a tool that never returned, or a hung model call). Send another message to continue.]")
				e.emitTurnError(closeCtx, in, tr, context.Cause(ctx))
			}
			e.Logger.Warn("runtime: turn interrupted before completion",
				slog.String("turn_id", tr.ID),
				slog.String("session_id", in.SessionID),
				slog.String("app_id", in.AppID),
				slog.String("reason", reason),
				slog.Any("cause", context.Cause(ctx)))
			if closeErr := tr.Interrupt(closeCtx, reason); closeErr != nil {
				e.Logger.Warn("runtime: tr.Interrupt emit error",
					slog.String("turn_id", tr.ID),
					slog.String("err", closeErr.Error()))
			}
			return nil, runErr
		}
		if closeErr := tr.Fail(closeCtx, runErr); closeErr != nil {
			e.Logger.Warn("runtime: tr.Fail emit error",
				slog.String("turn_id", tr.ID),
				slog.String("err", closeErr.Error()))
		}
		e.emitTurnError(closeCtx, in, tr, runErr)
		errRes := e.fireHook(closeCtx, in.AppID, agent, schema.HookEventError, hooks.Payload{
			AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID,
			TurnID: tr.ID, ErrorType: runErr.Error(),
		})
		e.applyInjections(closeCtx, in, tr, errRes.Injects)
		return nil, runErr
	}
	if err := tr.CloseDone(ctx); err != nil {
		e.Logger.Warn("runtime: tr.CloseDone emit error",
			slog.String("turn_id", tr.ID),
			slog.String("err", err.Error()))
	}
	turnEndRes := e.fireHook(ctx, in.AppID, agent, schema.HookEventTurnEnd, withTurnState(hooks.Payload{
		AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID, TurnID: tr.ID,
	}, endMetrics))
	e.applyInjections(ctx, in, tr, turnEndRes.Injects)
	res.TurnID = tr.ID
	return res, nil
}

func (e *Engine) runPhases(
	ctx context.Context,
	tr *turn.Turn,
	app *appmgr.RuntimeApp,
	agent *schema.Agent,
	snap sessionstore.SessionSnapshot,
	in TurnInput,
) (*TurnResult, hooks.Payload, error) {
	ctx = llm.WithUserJWT(ctx, in.UserJWT)
	if err := tr.TransitionTo(ctx, turn.PhaseLoading); err != nil {
		return nil, hooks.Payload{}, fmt.Errorf("runtime: phase loading: %w", err)
	}

	var (
		systemPrompt string
		tools        []llm.ToolSpec
	)
	if e.Context != nil {
		appName := ""
		appVersion := ""
		if app != nil {
			if app.Definition != nil {
				appName = app.Definition.App.Name
				appVersion = app.Definition.App.Version
			}
			if appName == "" && app.Meta != nil {
				appName = app.Meta.AppID
			}
		}
		memEnabled := appMemoryEnabled(app)
		agentEnabled := appAgentSpawnEnabled(app)
		var memView *prompt.WorkingMemoryView
		if memEnabled {
			memView = workingMemoryView(snap)
		}
		callAppEnabled, askUserWired, useSkillWired := false, false, false
		if pa, ok := e.Dispatcher.(primitiveAvailability); ok {
			callAppEnabled = pa.CallAppWired()
			askUserWired = pa.AskUserWired()
			useSkillWired = pa.UseSkillWired()
		}
		ctxRes, err := e.Context.BuildFor(ctx, ContextRequest{
			App:             app,
			Agent:           agent,
			AppName:         appName,
			AppVersion:      appVersion,
			MemoryEnabled:   memEnabled,
			AgentEnabled:    agentEnabled,
			CallAppEnabled:  callAppEnabled,
			AskUserEnabled:  askUserWired && appGrantsAskUser(app),
			UseSkillEnabled: useSkillWired && appHasSkills(app, agent),
			Memory:          memView,
		})
		if err != nil {
			return nil, hooks.Payload{}, fmt.Errorf("runtime: context_builder: %w", err)
		}
		systemPrompt = ctxRes.SystemPrompt
		tools = ctxRes.Tools
	} else {
		systemPrompt = agent.SystemPrompt
		if systemPrompt == "" {
			systemPrompt = agent.Prompt
		}
		tools = e.tools().ToolsForAgent(agent)
	}

	be := e.behaviorFor(app)
	if be != nil {
		be.OnTurnStart(in.SessionID)
	}

	mwPipe := e.middlewareFor(app)

	modeGate, modeMaxTurns, modeTimeout, behaviorProfile := e.applyTurnMode(ctx, tr, app, in, &snap, &tools)
	if modeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, time.Duration(modeTimeout*float64(time.Second)), errModeTimeout)
		defer cancel()
	}

	if be != nil {
		be.SetActiveProfile(in.SessionID, behaviorProfile)
		if bp := be.PromptText(in.SessionID); bp != "" {
			if systemPrompt != "" {
				systemPrompt += "\n\n" + bp
			} else {
				systemPrompt = bp
			}
		}
	}

	if e.PathPolicies != nil {
		if pp, ok := e.PathPolicies.PathPolicyFor(in.AppID, in.SessionID); ok && pp.HasWorkdir() {
			block := "# Working directory\n" +
				"You are working in: " + pp.Root() + "\n" +
				"All file paths are relative to this directory. You can only read and write files inside it — access to anything outside is denied."
			if systemPrompt != "" {
				systemPrompt += "\n\n" + block
			} else {
				systemPrompt = block
			}
			if projCaps := loadProjectCaps(pp.Root()); projCaps != nil {
				ctx = WithProjectCaps(ctx, projCaps)
			}
		}
	}

	if snap.ContextExtra != "" {
		if systemPrompt != "" {
			systemPrompt += "\n\n" + snap.ContextExtra
		} else {
			systemPrompt = snap.ContextExtra
		}
	}

	if cs := e.contextSectionsText(in, agent, app, snap); cs != "" {
		if systemPrompt != "" {
			systemPrompt += "\n\n" + cs
		} else {
			systemPrompt = cs
		}
	}

	// Selected starter template: fold its instructions into the authoritative
	// system prompt (not only the durable conversation directive) so even a
	// weak model treats them as binding on the scaffolding turn.
	if in.Template != "" && app.Definition != nil {
		for i := range app.Definition.Templates {
			if app.Definition.Templates[i].ID != in.Template {
				continue
			}
			if tp := strings.TrimSpace(app.Definition.Templates[i].SystemPrompt); tp != "" {
				block := "# Active starter template: " + app.Definition.Templates[i].Name + "\n" + tp
				if systemPrompt != "" {
					systemPrompt += "\n\n" + block
				} else {
					systemPrompt = block
				}
			}
			break
		}
	}

	var (
		apiKey  string
		baseURL string
	)
	if app.Meta != nil && app.Meta.BYOK {
		// BYOK: the brain-declared bundle credential is the fallback; a key the
		// user stored in their own vault for this provider takes precedence. The
		// resolver is an O(1) in-memory lookup (decrypts once, then cached) so
		// this never blocks the turn. Non-BYOK is untouched → gateway as before.
		apiKey, baseURL = extractEmbeddedAuth(agent.Brain)
		if e.CredResolver != nil {
			if uk, ub, ok := e.CredResolver.Lookup(ctx, in.UserID, agent.Brain.Provider); ok {
				apiKey = uk
				if ub != "" {
					baseURL = ub
				}
			}
		}
	}

	if err := tr.TransitionTo(ctx, turn.PhaseRunning); err != nil {
		return nil, hooks.Payload{}, fmt.Errorf("runtime: phase running: %w", err)
	}

	maxIter := e.MaxToolIterations
	if maxIter <= 0 {
		maxIter = defaultMaxToolIterations
	}
	if agent.MaxToolIterations != nil && *agent.MaxToolIterations > 0 {
		maxIter = *agent.MaxToolIterations
	}
	if modeMaxTurns > 0 && modeMaxTurns < maxIter {
		maxIter = modeMaxTurns
	}
	maxStopHold := resolveMaxStopVetoes(app.Definition.Runtime)

	if be != nil && be.ClassifyEnabled() {
		e.runBehaviorClassifier(ctx, tr, app, agent, be, in, &snap, tools, apiKey, baseURL)
	}

	e.injectSkillDirective(ctx, in, tr.ID, &snap)
	e.injectTemplateDirective(ctx, in, tr.ID, app, &snap)

	var (
		lastSeq       uint64
		lastContent   string
		lastModel     string
		usage         llm.Usage
		toolCallsUsed int
		finalAnswer   bool
		stopVetoes    int
	)
	emergencyCompacted := false

	conv := adapter.NewConverter(adapter.Options{
		LoadBlob:   e.loadBlob,
		Report:     &slogReporter{log: e.Logger},
		ExtractDoc: docextract.CachedExtract,
		// Attachments are ALWAYS hydrated inline: text inline, PDF/DOCX via
		// ExtractDoc → text, images → vision. (An earlier "drop + read on demand"
		// mode broke PDF reading — filesystem.read has no doc extraction — and
		// left agents without a session-context block unaware of the file.) The
		// files are also materialised to <workdir>/attachments/ for the Files
		// panel, but the model sees their content directly here.
	})
	compactPol := resolveAutoCompact(app.Definition.Runtime, agent.Brain.Context)
	guardKeep := compactPol.keep
	calibRatio := defaultEstimateRatio
	if cv := e.freshContextView(in.SessionID, snap, agent.Brain); cv.EstimateRatio > 0 {
		calibRatio = cv.EstimateRatio
	}
	for iter := 0; iter < maxIter; iter++ {
		if err := ctx.Err(); err != nil {
			return nil, hooks.Payload{}, fmt.Errorf("runtime: turn cancelled at iter %d: %w", iter, err)
		}
		tr.StepID = fmt.Sprintf("%s:s%d", tr.ID, iter)
		PingTurnKeepalive(ctx)

		e.guardContextPressure(ctx, in, agent, &snap, compactPol, &guardKeep, usage.PromptTokens)

		msgs := e.buildLLMMessages(ctx, conv, snap, systemPrompt, in.SessionID, agent.Brain)

		req := &llm.ChatRequest{
			BYOK:     app.Meta != nil && app.Meta.BYOK,
			Provider: agent.Brain.Provider,
			Model:    agent.Brain.Model,
			APIKey:   apiKey,
			BaseURL:  baseURL,
			UserJWT:  in.UserJWT,
			Messages: msgs,
			Tools:    tools,
			// Full gateway attribution : app + session + user + agent
			// instance (sub-agents carry their RunID) + the per-step id
			// as run correlation — billing/audit needs all of them.
			AppID:         in.AppID,
			SessionID:     in.SessionID,
			UserID:        in.UserID,
			AgentID:       agentRunID(in.AgentRunID, agent.ID),
			CorrelationID: tr.StepID,
		}
		e.recordContextParts(in.SessionID, systemPrompt, tools)
		if agent.Brain.Temperature != nil {
			req.Temperature = agent.Brain.Temperature
		}
		if agent.Brain.MaxTokens != nil {
			req.MaxTokens = agent.Brain.MaxTokens
		}
		if agent.Brain.Timeout != nil && *agent.Brain.Timeout > 0 {
			req.Timeout = time.Duration(*agent.Brain.Timeout * float64(time.Second))
		}
		if eff := e.reasoningEffortFor(in.SessionID, agent.ID); eff != "" {
			req.ReasoningEffort = eff
		}

		var (
			mctx           *ports.MiddlewareContext
			shortCircuited bool
			resp           *llm.ChatResponse
			sentEst        int
		)
		if mwPipe != nil {
			mctx = buildMiddlewareContext(agent, in, iter, systemPrompt, req.Messages)
			scResp, sc, mwErr := mwPipe.Before(ctx, mctx)
			if mwErr != nil {
				return nil, hooks.Payload{}, fmt.Errorf("runtime: middleware before (iter %d): %w", iter, mwErr)
			}
			applyMiddlewareContext(req.Messages, mctx, systemPrompt)
			if sc {
				shortCircuited = true
				resp = &llm.ChatResponse{Content: scResp, Model: "middleware"}
			}
		}

		if !shortCircuited {
			e.enforcePromptBudget(ctx, in, agent, conv, &snap, req, systemPrompt, compactPol, &guardKeep, calibRatio)
			sentEst = estReqTokens(req)
			var r *llm.ChatResponse
			var err error
			isMediaAgent := agent.Brain.Kind == "image" || agent.Brain.Kind == "video"
			for retries := 0; ; {
				if isMediaAgent {
					r, err = e.generateMedia(ctx, in, agent, req)
				} else {
					r, err = e.chatOrStream(ctx, tr, in, req)
				}
				if err != nil && e.Compactor != nil && !emergencyCompacted && contextcompact.IsContextOverflow(err) {
					emergencyCompacted = true
					keep := 0
					if agent.Brain.Context != nil {
						keep = agent.Brain.Context.KeepRecent
					}
					if cerr := e.Compactor.CompactSession(ctx, in.SessionID, contextcompact.StrategyTruncate, contextcompact.EmergencyKeepRecent(keep)); cerr == nil {
						if st, serr := e.Sessions.State(in.SessionID); serr == nil && st != nil {
							snap = st.Snapshot()
							vm := snap.Messages
							if cc := snap.ContextCompaction; cc != nil && cc.CutoffSeq > 0 {
								recap := cc.Summary
								if len(snap.Facts) > 0 {
									recap = contextcompact.StripKeyFactsSection(recap)
								}
								vm = contextcompact.ApplyView(snap.Messages, cc.CutoffSeq, recap)
							}
							req.Messages = conv.Convert(ctx, vm)
							if systemPrompt != "" {
								req.Messages = append([]llm.ChatMessage{{Role: "system", Content: systemPrompt}}, req.Messages...)
							}
							snipOversizedMessages(req.Messages, e.msgByteCap(in.SessionID, snap, agent.Brain))
							r, err = e.chatOrStream(ctx, tr, in, req)
						}
					}
				}
				if err == nil || retries >= maxTurnRetries || !transientRetryable(err, r) {
					break
				}
				retries++
				delay := turnRetryBackoff(retries)
				e.emitTurnRetry(in, tr, err, retries, delay)
				select {
				case <-ctx.Done():
				case <-time.After(delay):
				}
				if ctx.Err() != nil {
					err = ctx.Err()
					break
				}
			}
			if err != nil {
				if r != nil && strings.TrimSpace(r.Content) != "" {
					e.persistInterruptedAssistant(tr, in,
						r.Content+interruptMarker(err))
				}
				return nil, hooks.Payload{}, fmt.Errorf("runtime: llm chat (iter %d): %w", iter, err)
			}
			if r == nil {
				return nil, hooks.Payload{}, fmt.Errorf("runtime: llm returned nil response (iter %d)", iter)
			}
			resp = r
		}

		if e.ResponseNormalizer != nil {
			e.ResponseNormalizer(resp)
		}

		if clean, reasoning := splitInlineReasoning(resp.Content); reasoning != "" {
			resp.Content = clean
			if resp.ReasoningContent == "" {
				resp.ReasoningContent = reasoning
			} else {
				resp.ReasoningContent = resp.ReasoningContent + "\n" + reasoning
			}
		}

		if mwPipe != nil {
			out, mwErr := mwPipe.After(ctx, mctx, resp.Content, toPortsToolCalls(resp.ToolCalls))
			if mwErr != nil {
				return nil, hooks.Payload{}, fmt.Errorf("runtime: middleware after (iter %d): %w", iter, mwErr)
			}
			resp.Content = out
		}

		canonicalizeToolCallNames(resp.ToolCalls, tools)

		seq, err := e.persistAssistantStep(ctx, tr, in, resp)
		if err != nil {
			return nil, hooks.Payload{}, err
		}
		lastSeq = seq
		lastContent = resp.Content
		lastModel = resp.Model
		usage = resp.Usage

		if sentEst > 0 && usage.PromptTokens > 0 {
			r := float64(usage.PromptTokens) / float64(sentEst)
			if r < 1.0 {
				r = 1.0
			} else if r > 4.0 {
				r = 4.0
			}
			calibRatio = r
			if e.ContextRecordRatio != nil {
				e.ContextRecordRatio(in.SessionID, r)
			}
		}

		var beNotes []behavior.Violation
		if be != nil {
			beNotes = append(beNotes, be.OnAgentText(in.SessionID, resp.Content)...)
		}

		if len(resp.ToolCalls) == 0 {
			e.injectBehaviorNotes(ctx, in, tr, beNotes)
			if stopVetoes < maxStopHold {
				stopRes := e.fireHook(ctx, in.AppID, agent, schema.HookEventStop,
					withTurnState(hooks.Payload{
						AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID, TurnID: tr.ID,
					}, e.computeHookMetrics(snap, agent, resp.Content, toolCallsUsed)))
				if stopRes.Gate != nil && !stopRes.Gate.Allow {
					stopVetoes++
					if len(stopRes.Injects) > 0 {
						e.applyInjections(ctx, in, tr, stopRes.Injects)
					} else if r := strings.TrimSpace(stopRes.Gate.Reason); r != "" {
						e.injectSystemDirective(ctx, in, tr.ID,
							wrapRuntimeDirective("stop", "critical", r),
							DirectiveHookInject, map[string]any{"hook": "stop"}, nil)
					}
					if st, sErr := e.Sessions.State(in.SessionID); sErr == nil && st != nil {
						snap = st.Snapshot()
					}
					e.Logger.Info("runtime: stop hook held the turn open",
						slog.String("session_id", in.SessionID),
						slog.Int("veto", stopVetoes))
					continue
				}
			} else {
				e.Logger.Warn("runtime: stop-veto cap reached; ending turn",
					slog.String("session_id", in.SessionID),
					slog.Int("cap", maxStopHold))
			}
			finalAnswer = true
			break
		}

		e.persistToolCallEvents(ctx, tr, in, resp.ToolCalls)

		toolCallsUsed += len(resp.ToolCalls)
		metrics := e.computeHookMetrics(snap, agent, resp.Content, toolCallsUsed)

		var beBlocks map[int]string
		if be != nil {
			beBlocks = map[int]string{}
			for i, tc := range resp.ToolCalls {
				for _, v := range be.PreTool(in.SessionID, tc.Name, tc.Arguments, resp.Content) {
					if v.Level == "block" {
						beBlocks[i] = v.Format()
					} else {
						beNotes = append(beNotes, v)
					}
				}
			}
		}

		outcomes := e.dispatchToolsParallel(ctx, tr, in, app, agent, resp.ToolCalls, metrics, modeGate, beBlocks)

		PingTurnKeepalive(ctx)

		if err := e.persistToolResults(ctx, tr, in, resp.ToolCalls, outcomes); err != nil {
			return nil, hooks.Payload{}, err
		}

		if be != nil {
			for i, tc := range resp.ToolCalls {
				if _, blocked := beBlocks[i]; blocked {
					continue
				}
				beNotes = append(beNotes, be.PostTool(in.SessionID, tc.Name, tc.Arguments, outcomeToResult(outcomes[i]))...)
			}
		}
		e.injectBehaviorNotes(ctx, in, tr, beNotes)

		if st, sErr := e.Sessions.State(in.SessionID); sErr == nil && st != nil {
			snap = st.Snapshot()
		}
	}

	if !finalAnswer {
		note := fmt.Sprintf("[Turn stopped after %d tool steps without finishing — the task may be incomplete. Send another message to continue. (An app can raise agents[].max_tool_iterations.)]", maxIter)
		if strings.TrimSpace(lastContent) != "" {
			note = lastContent + "\n\n" + note
		}
		tr.StepID = fmt.Sprintf("%s:s%d", tr.ID, maxIter)
		if seq, perr := e.persistAssistantStep(ctx, tr, in, &llm.ChatResponse{Content: note}); perr == nil {
			lastSeq = seq
		}
		lastContent = note
		e.Logger.Warn("runtime: turn hit tool-iteration limit",
			slog.String("session_id", in.SessionID),
			slog.String("app_id", in.AppID),
			slog.Int("max_iter", maxIter))
	}

	if err := tr.TransitionTo(ctx, turn.PhasePersisting); err != nil {
		return nil, hooks.Payload{}, fmt.Errorf("runtime: phase persisting: %w", err)
	}

	e.Logger.Info("runtime: turn complete",
		slog.String("turn_id", tr.ID),
		slog.String("app_id", in.AppID),
		slog.String("session_id", in.SessionID),
		slog.String("model", lastModel),
		slog.Int("tokens_in", usage.PromptTokens),
		slog.Int("tokens_out", usage.CompletionTokens),
		slog.Uint64("seq", lastSeq),
	)
	e.emitTokenUsage(ctx, in, tr.ID, usage)
	e.touchContext(in.SessionID)
	if e.PrepareSummary != nil {
		e.PrepareSummary(in.SessionID, llm.UserJWTFromContext(ctx))
	}
	endMetrics := e.computeHookMetrics(snap, agent, lastContent, toolCallsUsed)
	return &TurnResult{Seq: lastSeq, Content: lastContent}, endMetrics, nil
}

func (e *Engine) touchContext(sessionID string) {
	if e.ContextTouch != nil {
		e.ContextTouch(sessionID)
	}
}

func (e *Engine) PreWarmSession(sessionID, appID string) {
	if e.Apps == nil || e.Context == nil || e.ContextRecordParts == nil {
		return
	}
	app, err := e.Apps.Get(context.Background(), appID)
	if err != nil || app == nil || app.Definition == nil || len(app.Definition.Agents) == 0 {
		return
	}
	agent := &app.Definition.Agents[0]
	if app.Definition.Runtime != nil && app.Definition.Runtime.EntryAgent != "" {
		for i := range app.Definition.Agents {
			if app.Definition.Agents[i].ID == app.Definition.Runtime.EntryAgent {
				agent = &app.Definition.Agents[i]
				break
			}
		}
	}
	res, err := e.Context.BuildFor(context.Background(), ContextRequest{
		App:     app,
		Agent:   agent,
		AppName: app.Definition.App.Name,
	})
	if err != nil || res.SystemPrompt == "" {
		return
	}
	e.recordContextParts(sessionID, res.SystemPrompt, res.Tools)
}

func (e *Engine) recordContextParts(sessionID, systemPrompt string, tools []llm.ToolSpec) {
	if e.ContextRecordParts == nil {
		return
	}
	var sys []string
	if systemPrompt != "" {
		sys = []string{systemPrompt}
	}
	var toolTexts []string
	for i := range tools {
		if b, err := json.Marshal(tools[i]); err == nil {
			toolTexts = append(toolTexts, string(b))
		}
	}
	e.ContextRecordParts(sessionID, sys, toolTexts)
}

func (e *Engine) freshContextView(sessionID string, snap sessionstore.SessionSnapshot, brain schema.Brain) contextsvc.ContextView {
	resolveGW := func() int {
		if e.ModelWindowLookup != nil {
			if gw := e.ModelWindowLookup(brain.Model); gw > 0 {
				return gw
			}
		}
		return snap.EntryModelWindow
	}
	if e.ContextLookup != nil {
		if cv, ok := e.ContextLookup(sessionID); ok && cv.Used > 0 {
			expected := contextsvc.ViewFromSnapshotWithRuntimeAndGateway(snap, brain, e.runtimeMaxTokens(snap.AppID), resolveGW())
			if cv.Limit == expected.Limit {
				return cv
			}
		}
	}
	return contextsvc.ViewFromSnapshotWithRuntimeAndGateway(snap, brain, e.runtimeMaxTokens(snap.AppID), resolveGW())
}

func (e *Engine) runtimeMaxTokens(appID string) int {
	if e.Apps == nil || appID == "" {
		return 0
	}
	rt, err := e.Apps.Get(context.Background(), appID)
	if err != nil || rt == nil || rt.Definition == nil || rt.Definition.Runtime == nil || rt.Definition.Runtime.Context == nil {
		return 0
	}
	return rt.Definition.Runtime.Context.MaxTokens
}

func (e *Engine) msgByteCap(sessionID string, snap sessionstore.SessionSnapshot, brain schema.Brain) int {
	capBytes := maxMessageBytes
	cv := e.freshContextView(sessionID, snap, brain)
	if cv.Limit > 0 {
		if budget := cv.Limit - cv.System - cv.Tools - 256; budget > 256 {
			if b := budget * safetyCharsPerToken; b < capBytes {
				capBytes = b
			}
		}
	}
	return capBytes
}

const (
	microCompactKeepRecent = 3
	microCompactMinBytes   = 4096
)

func (e *Engine) buildLLMMessages(ctx context.Context, conv *adapter.Converter, snap sessionstore.SessionSnapshot, systemPrompt, sessionID string, brain schema.Brain) []llm.ChatMessage {
	viewMsgs := snap.Messages
	if cc := snap.ContextCompaction; cc != nil && cc.CutoffSeq > 0 {
		recap := cc.Summary
		if len(snap.Facts) > 0 {
			recap = contextcompact.StripKeyFactsSection(recap)
		}
		viewMsgs = contextcompact.ApplyView(snap.Messages, cc.CutoffSeq, recap)
	}
	if e.MicroCompactView {
		viewMsgs = contextcompact.MicroCompact(viewMsgs, microCompactKeepRecent, microCompactMinBytes)
	}
	msgs := conv.Convert(ctx, viewMsgs)
	snipOversizedMessages(msgs, e.msgByteCap(sessionID, snap, brain))
	if systemPrompt != "" {
		msgs = append([]llm.ChatMessage{{Role: "system", Content: systemPrompt}}, msgs...)
	}
	return msgs
}

func estReqTokens(req *llm.ChatRequest) int {
	chars := 0
	for i := range req.Messages {
		if b, err := json.Marshal(req.Messages[i]); err == nil {
			chars += len(b)
		}
	}
	for i := range req.Tools {
		if b, err := json.Marshal(req.Tools[i]); err == nil {
			chars += len(b)
		}
	}
	return chars / safetyCharsPerToken
}

const safetyCharsPerToken = 3

func (e *Engine) enforcePromptBudget(
	ctx context.Context, in TurnInput, agent *schema.Agent, conv *adapter.Converter,
	snap *sessionstore.SessionSnapshot, req *llm.ChatRequest, systemPrompt string,
	pol autoCompactPolicy, keep *int, ratio float64,
) {
	if e.Compactor == nil {
		return
	}
	cv := e.freshContextView(in.SessionID, *snap, agent.Brain)
	limit := cv.Limit
	if limit <= 0 {
		return
	}
	if ratio < 1 {
		ratio = defaultEstimateRatio
	}
	effLimit := int(float64(limit) / ratio)
	if effLimit < 256 {
		effLimit = 256
	}
	strategy := pol.strategy
	if strategy == "" {
		strategy = contextcompact.StrategyTruncate
	}
	prev := estReqTokens(req)
	for attempt := 0; attempt < 8 && prev > effLimit; attempt++ {
		k := contextcompact.KeepRecentOrDefault(*keep)
		if err := e.Compactor.CompactSession(ctx, in.SessionID, strategy, k); err != nil {
			return
		}
		if nk := k / 2; nk >= 2 {
			*keep = nk
		} else {
			*keep = 2
		}
		st, serr := e.Sessions.State(in.SessionID)
		if serr != nil || st == nil {
			return
		}
		*snap = st.Snapshot()
		req.Messages = e.buildLLMMessages(ctx, conv, *snap, systemPrompt, in.SessionID, agent.Brain)
		cur := estReqTokens(req)
		if cur >= prev {
			e.Logger.Warn("runtime: prompt still over budget after max compaction",
				slog.String("session_id", in.SessionID),
				slog.Int("est", cur), slog.Int("eff_limit", effLimit))
			return
		}
		prev = cur
	}
	if prev > effLimit {
		return
	}
	e.touchContext(in.SessionID)
}

const defaultEstimateRatio = 1.6

type autoCompactPolicy struct {
	on        bool
	threshold float64
	keep      int
	strategy  string
}

func resolveAutoCompact(rt *schema.RuntimeBlock, brainCtx *schema.ContextConfig) autoCompactPolicy {
	p := autoCompactPolicy{on: true, strategy: contextcompact.StrategySummarize}
	var rc *schema.ContextConfig
	if rt != nil {
		rc = rt.Context
	}
	if rc != nil {
		if rc.AutoCompact != nil && !*rc.AutoCompact {
			p.on = false
		}
		if rc.CompressionTrigger > 0 {
			p.threshold = rc.CompressionTrigger
		}
		if rc.KeepRecent > 0 {
			p.keep = rc.KeepRecent
		}
		if rc.Strategy != "" {
			p.strategy = string(rc.Strategy)
		}
	}
	if p.threshold == 0 && brainCtx != nil && brainCtx.CompressionTrigger > 0 {
		p.threshold = brainCtx.CompressionTrigger
	}
	if p.keep == 0 && brainCtx != nil && brainCtx.KeepRecent > 0 {
		p.keep = brainCtx.KeepRecent
	}
	if p.threshold == 0 {
		p.threshold = 0.95
	}
	return p
}

const (
	compactionAbsBuffer     = 13000
	compactionMaxBufferFrac = 0.25
)

func compactionTriggerPoint(limit int, threshold float64) int {
	if limit <= 0 {
		return 0
	}
	ratioPoint := int(float64(limit) * threshold)
	buffer := compactionAbsBuffer
	if maxBuf := int(float64(limit) * compactionMaxBufferFrac); buffer > maxBuf {
		buffer = maxBuf
	}
	bufferPoint := limit - buffer
	if bufferPoint < ratioPoint {
		return bufferPoint
	}
	return ratioPoint
}

func (e *Engine) guardContextPressure(
	ctx context.Context, in TurnInput, agent *schema.Agent,
	snap *sessionstore.SessionSnapshot, pol autoCompactPolicy, keep *int, lastPromptTokens int,
) bool {
	if !pol.on || e.Compactor == nil || pol.threshold <= 0 {
		return false
	}
	cv := e.freshContextView(in.SessionID, *snap, agent.Brain)
	if cv.Limit <= 0 {
		return false
	}
	used := cv.Used
	if lastPromptTokens > used {
		used = lastPromptTokens
	}
	if used < compactionTriggerPoint(cv.Limit, pol.threshold) {
		return false
	}
	var replayMsg *sessionstore.Message
	for i := len(snap.Messages) - 1; i >= 0; i-- {
		if snap.Messages[i].Role == "user" {
			m := snap.Messages[i]
			replayMsg = &m
			break
		}
	}

	k := contextcompact.KeepRecentOrDefault(*keep)
	if cerr := e.Compactor.CompactSession(ctx, in.SessionID, pol.strategy, k); cerr != nil {
		e.Logger.Warn("runtime: per-round context guard compaction failed (non-fatal)",
			slog.String("session_id", in.SessionID), slog.String("err", cerr.Error()))
		return false
	}
	if st, serr := e.Sessions.State(in.SessionID); serr == nil && st != nil {
		*snap = st.Snapshot()
	}

	if replayMsg != nil && replayMsg.Seq > 0 && e.Sessions != nil {
		found := false
		for i := range snap.Messages {
			if snap.Messages[i].Seq == replayMsg.Seq {
				found = true
				break
			}
		}
		if !found {
			ev := sessionstore.Event{
				Type:      sessionstore.EventUserMessage,
				SessionID: in.SessionID,
				AppID:     in.AppID,
				UserID:    in.UserID,
				Message: &sessionstore.MessagePayload{
					Role:  replayMsg.Role,
					Parts: replayMsg.Parts,
				},
			}
			if ev.Message.Parts == nil && replayMsg.Content != "" {
				ev.Message.Parts = []sessionstore.MessagePart{
					{Type: sessionstore.PartTypeText, Text: replayMsg.Content},
				}
			}
			if _, rerr := e.Sessions.AppendDurable(ctx, ev); rerr == nil {
				if st, serr := e.Sessions.State(in.SessionID); serr == nil && st != nil {
					*snap = st.Snapshot()
				}
				e.Logger.Info("runtime: replayed user message after compaction dropped it",
					slog.String("session_id", in.SessionID),
					slog.Uint64("original_seq", replayMsg.Seq))
			}
		}
	}

	if nk := k / 2; nk >= 2 {
		*keep = nk
	} else {
		*keep = 2
	}
	e.touchContext(in.SessionID)
	e.Logger.Info("runtime: per-round context guard compacted",
		slog.String("session_id", in.SessionID),
		slog.Int("used", used), slog.Int("limit", cv.Limit),
		slog.Int("kept", k))
	return true
}

func (e *Engine) emitTokenUsage(ctx context.Context, in TurnInput, turnID string, usage llm.Usage) {
	if usage.PromptTokens <= 0 && usage.CompletionTokens <= 0 {
		return
	}
	if e.Sessions == nil {
		return
	}
	_, err := e.Sessions.AppendDurable(ctx, sessionstore.Event{
		Type:          sessionstore.EventTokenUsage,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: turnID,
		Cost: &sessionstore.CostPayload{
			TokensIn:         int64(usage.PromptTokens),
			TokensOut:        int64(usage.CompletionTokens),
			ReasoningTokens:  int64(usage.ReasoningTokens),
			CacheReadTokens:  int64(usage.CacheReadTokens),
			CacheWriteTokens: int64(usage.CacheWriteTokens),
		},
	})
	if err != nil {
		e.Logger.Warn("runtime: persist token usage failed (non-fatal)",
			slog.String("session_id", in.SessionID),
			slog.String("err", err.Error()))
	}
}

func (e *Engine) persistAssistantStep(
	ctx context.Context, tr *turn.Turn, in TurnInput, resp *llm.ChatResponse,
) (uint64, error) {
	ev := sessionstore.Event{
		Type:          sessionstore.EventAssistantMessage,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: tr.ID,
		StepID:        tr.StepID,
		Message: &sessionstore.MessagePayload{
			Role:               "assistant",
			Content:            resp.Content,
			Parts:              e.buildAssistantParts(ctx, resp),
			Reasoning:          resp.ReasoningContent,
			ReasoningStartedAt: resp.ReasoningStartedAt,
			ReasoningEndedAt:   resp.ReasoningEndedAt,
		},
	}
	seq, err := e.Sessions.AppendDurable(ctx, ev)
	if err != nil {
		return 0, fmt.Errorf("runtime: persist assistant message: %w", err)
	}
	return seq, nil
}

func (e *Engine) emitTurnError(ctx context.Context, in TurnInput, tr *turn.Turn, cause error) {
	info := errclass.Classify(cause)
	retry := info.Retry
	ev := sessionstore.Event{
		Type:          sessionstore.EventError,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: tr.ID,
		Error: &sessionstore.ErrorPayload{
			Error:    info.Error,
			Message:  info.Error,
			Code:     info.Code,
			Category: info.Category,
			Detail:   info.Detail,
			Retry:    &retry,
			Source:   "turn",
		},
	}
	if _, err := e.Sessions.AppendDurable(ctx, ev); err != nil && e.Logger != nil {
		e.Logger.Warn("runtime: emit turn error event failed",
			slog.String("turn_id", tr.ID), slog.String("err", err.Error()))
	}
}

const maxTurnRetries = 3

func turnRetryBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := time.Second << (n - 1)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

func transientRetryable(err error, r *llm.ChatResponse) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if r != nil && strings.TrimSpace(r.Content) != "" {
		return false
	}
	return errclass.Classify(err).Retry
}

func (e *Engine) emitTurnRetry(in TurnInput, tr *turn.Turn, cause error, attempt int, delay time.Duration) {
	info := errclass.Classify(cause)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ev := sessionstore.Event{
		Type:          sessionstore.EventTurnRetry,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: tr.ID,
		Retry: &sessionstore.RetryPayload{
			Attempt:   attempt + 1,
			Max:       maxTurnRetries + 1,
			Message:   info.Error,
			Code:      info.Code,
			Category:  info.Category,
			RetryInMs: int(delay / time.Millisecond),
		},
	}
	if _, err := e.Sessions.AppendDurable(ctx, ev); err != nil && e.Logger != nil {
		e.Logger.Warn("runtime: emit turn_retry event failed",
			slog.String("turn_id", tr.ID), slog.String("err", err.Error()))
	}
}

func (e *Engine) modelOverrideFor(sessionID, agentID string, selfOverrides map[string]string) string {
	if i := strings.Index(sessionID, "::agent::"); i >= 0 {
		if st, err := e.Sessions.State(sessionID[:i]); err == nil && st != nil {
			return st.Snapshot().ModelOverrides[agentID]
		}
		return ""
	}
	return selfOverrides[agentID]
}

// providerOverrideFor mirrors modelOverrideFor for the cross-provider pin: the
// provider of a BYOK model that belongs to a different provider than the brain.
func (e *Engine) providerOverrideFor(sessionID, agentID string, selfOverrides map[string]string) string {
	if i := strings.Index(sessionID, "::agent::"); i >= 0 {
		if st, err := e.Sessions.State(sessionID[:i]); err == nil && st != nil {
			return st.Snapshot().ProviderOverrides[agentID]
		}
		return ""
	}
	return selfOverrides[agentID]
}

// outputOverrideFor mirrors modelOverrideFor for the per-model max generation
// tokens the user pinned (BYOK models whose limits the gateway doesn't know).
func (e *Engine) outputOverrideFor(sessionID, agentID string, selfOverrides map[string]int) int {
	if i := strings.Index(sessionID, "::agent::"); i >= 0 {
		if st, err := e.Sessions.State(sessionID[:i]); err == nil && st != nil {
			return st.Snapshot().OutputTokenOverrides[agentID]
		}
		return 0
	}
	return selfOverrides[agentID]
}

func (e *Engine) reasoningEffortFor(sessionID, agentID string) string {
	root := sessionID
	if i := strings.Index(sessionID, "::agent::"); i >= 0 {
		root = sessionID[:i]
	}
	if st, err := e.Sessions.State(root); err == nil && st != nil {
		snap := st.Snapshot()
		if ov := snap.EffortOverrides[agentID]; ov != "" {
			return ov
		}
		return snap.ReasoningEffort
	}
	return ""
}

func interruptMarker(cause error) string {
	reason := "generation was cut off before finishing"
	switch {
	case errors.Is(cause, context.Canceled):
		reason = "generation was stopped here on request"
	case errors.Is(cause, context.DeadlineExceeded):
		reason = "generation timed out before finishing"
	default:
		switch errclass.Classify(cause).Category {
		case "network":
			reason = "the connection to the model provider dropped mid-generation"
		case "rate_limit":
			reason = "the model provider rate-limited the request mid-generation"
		case "provider":
			reason = "the model provider returned an error mid-generation"
		}
	}
	return "\n\n[Response interrupted before completion — " + reason + ". Continue from this point if the task is unfinished.]"
}

func (e *Engine) persistInterruptedAssistant(tr *turn.Turn, in TurnInput, content string) {
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ev := sessionstore.Event{
		Type:          sessionstore.EventAssistantMessage,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: tr.ID,
		StepID:        tr.StepID,
		Message: &sessionstore.MessagePayload{
			Role:    "assistant",
			Content: content,
			Parts:   []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: content}},
		},
	}
	if _, err := e.Sessions.AppendDurable(saveCtx, ev); err != nil && e.Logger != nil {
		e.Logger.Warn("runtime: persist interrupted partial failed",
			slog.String("turn_id", tr.ID), slog.String("err", err.Error()))
	}
}

func canonicalizeToolCallNames(calls []llm.ChatToolCall, offered []llm.ToolSpec) {
	known := make([]string, 0, len(offered))
	alias := make(map[string]string, len(offered))
	singleWire := make(map[string]string, len(offered))
	addSingleWire := func(fqn string) {
		dot := strings.IndexByte(fqn, '.')
		if dot < 0 {
			return
		}
		w := fqn[:dot] + "_" + fqn[dot+1:]
		if prev, seen := singleWire[w]; seen && prev != fqn {
			singleWire[w] = ""
		} else {
			singleWire[w] = fqn
		}
	}
	for _, t := range offered {
		if t.Canonical != "" {
			alias[t.Name] = t.Canonical
			known = append(known, t.Canonical)
			addSingleWire(t.Canonical)
			continue
		}
		fqn := toolname.Canonicalize(t.Name)
		known = append(known, fqn)
		addSingleWire(fqn)
	}
	for i := range calls {
		if canon, ok := alias[calls[i].Name]; ok {
			calls[i].Name = canon
			continue
		}
		name := toolname.ResolveAlias(toolname.Canonicalize(calls[i].Name))
		name = toolname.QualifyBareName(name, known)
		if !strings.Contains(name, ".") {
			if fqn := singleWire[name]; fqn != "" {
				name = fqn
			}
		}
		calls[i].Name = name
	}
}

func buildMiddlewareContext(agent *schema.Agent, in TurnInput, turn int, systemPrompt string, msgs []llm.ChatMessage) *ports.MiddlewareContext {
	agentID := ""
	if agent != nil {
		agentID = agent.ID
	}
	pm := make([]ports.LLMMessage, len(msgs))
	for i := range msgs {
		pm[i] = ports.LLMMessage{Role: msgs[i].Role, Content: msgs[i].Content}
	}
	return &ports.MiddlewareContext{
		AgentID: agentID, SessionID: in.SessionID, UserID: in.UserID,
		Turn: turn, SystemPrompt: systemPrompt, Messages: pm, Metadata: map[string]any{},
	}
}

func applyMiddlewareContext(msgs []llm.ChatMessage, mctx *ports.MiddlewareContext, systemPrompt string) {
	n := len(msgs)
	if len(mctx.Messages) < n {
		n = len(mctx.Messages)
	}
	for i := 0; i < n; i++ {
		msgs[i].Content = mctx.Messages[i].Content
	}
	if systemPrompt != "" && len(msgs) > 0 && msgs[0].Role == "system" {
		msgs[0].Content = mctx.SystemPrompt
	}
}

func toPortsToolCalls(calls []llm.ChatToolCall) []ports.LLMToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ports.LLMToolCall, len(calls))
	for i := range calls {
		args := ""
		if calls[i].Arguments != nil {
			if b, err := json.Marshal(calls[i].Arguments); err == nil {
				args = string(b)
			}
		}
		out[i] = ports.LLMToolCall{ID: calls[i].ID, Name: calls[i].Name, Arguments: args}
	}
	return out
}

func (e *Engine) persistToolCallEvents(
	ctx context.Context, tr *turn.Turn, in TurnInput, calls []llm.ChatToolCall,
) {
	if len(calls) == 0 {
		return
	}
	evs := make([]sessionstore.Event, len(calls))
	for i, tc := range calls {
		evs[i] = sessionstore.Event{
			Type:          sessionstore.EventToolCall,
			SessionID:     in.SessionID,
			AppID:         in.AppID,
			UserID:        in.UserID,
			CorrelationID: tr.ID,
			StepID:        tr.StepID,
			Tool: &sessionstore.ToolPayload{
				CallID:    tc.ID,
				Name:      tc.Name,
				Arguments: maps.Clone(tc.Arguments),
				Status:    "pending",
			},
		}
	}
	if batcher, ok := e.Sessions.(durableBatcher); ok {
		if _, err := batcher.AppendDurableBatch(ctx, evs); err != nil {
			e.Logger.Warn("runtime: persist tool_call events batch failed",
				slog.Int("count", len(calls)),
				slog.String("err", err.Error()))
		}
		return
	}
	for i := range evs {
		if _, err := e.Sessions.AppendDurable(ctx, evs[i]); err != nil {
			e.Logger.Warn("runtime: persist tool_call event failed",
				slog.String("call_id", calls[i].ID),
				slog.String("err", err.Error()))
		}
	}
}

// stripIntentArg removes the narration-only `intent` key before a tool runs
// (ui.tool_calls.inject_intent). Returns the original map untouched when absent
// — so the flag-OFF path is byte-for-byte the previous behaviour.
func stripIntentArg(args map[string]any) map[string]any {
	if _, ok := args["intent"]; !ok {
		return args
	}
	clone := maps.Clone(args)
	delete(clone, "intent")
	return clone
}

func (e *Engine) dispatchToolsParallel(
	ctx context.Context, tr *turn.Turn, in TurnInput,
	app *appmgr.RuntimeApp, agent *schema.Agent,
	calls []llm.ChatToolCall, metrics hooks.Payload, gate *modeGuard,
	beBlocks map[int]string,
) []ToolOutcome {
	ctx = withModeGuard(ctx, gate)
	if e.PathPolicies != nil {
		if pp, ok := e.PathPolicies.PathPolicyFor(in.AppID, in.SessionID); ok {
			ctx = workdir.WithPathPolicy(ctx, pp)
		}
	}
	outcomes := make([]ToolOutcome, len(calls))
	var wg sync.WaitGroup
	wg.Add(len(calls))
	for i, tc := range calls {
		go func(i int, tc llm.ChatToolCall) {
			defer wg.Done()
			start := time.Now()
			defer func() {
				if r := recover(); r != nil {
					outcomes[i] = ToolOutcome{
						Status:     "errored",
						Error:      "tool=" + tc.Name + ": " + safego.Report("engine.tool:"+tc.Name, r),
						DurationMs: time.Since(start).Milliseconds(),
					}
				}
			}()

			if beBlocks != nil {
				if msg, blocked := beBlocks[i]; blocked {
					outcomes[i] = ToolOutcome{
						Status:     "errored",
						Error:      msg,
						DurationMs: time.Since(start).Milliseconds(),
					}
					return
				}
			}

			if gate != nil {
				if _, ok := gate.allowed[tc.Name]; !ok {
					outcomes[i] = ToolOutcome{
						Status: "errored",
						Error: fmt.Sprintf(
							"Tool %q is blocked in mode %q. Allowed tools: %s. Ask the user to switch to a mode that allows this tool. Do not retry this call.",
							tc.Name, gate.label, gate.allowedList),
						DurationMs: time.Since(start).Milliseconds(),
					}
					return
				}
			}

			if blocked := e.enforceGate(ctx, in.SessionID, in.AppID, in.UserID, tr.ID, app, agent, tc.Name, tc.ID, tc.Arguments, tr, &in); blocked != nil {
				blocked.DurationMs = time.Since(start).Milliseconds()
				outcomes[i] = *blocked
				return
			}

			agentID := ""
			if agent != nil {
				agentID = agent.ID
			}
			gateRes := e.fireHookGate(ctx, in.AppID, agent, schema.HookEventToolStart, withTurnState(hooks.Payload{
				AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID,
				TurnID: tr.ID, ToolName: tc.Name, ToolArgs: tc.Arguments,
			}, metrics))
			if gateRes != nil && !gateRes.Allow {
				outcomes[i] = ToolOutcome{
					Status:     "errored",
					Error:      "blocked by hook gate: " + gateRes.Reason,
					DurationMs: time.Since(start).Milliseconds(),
				}
				return
			}
			dctx := ctx
			var toolCancel context.CancelFunc
			var stopTicker chan struct{}
			if isLongRunningTool(tc.Name) {
				stopTicker = make(chan struct{})
				go keepaliveTicker(ctx, stopTicker)
			} else if e.ToolTimeout > 0 {
				dctx, toolCancel = context.WithTimeout(ctx, e.ToolTimeout)
			}
			args := tc.Arguments
			if app.InjectIntent() {
				args = stripIntentArg(tc.Arguments)
			}
			outcomes[i] = e.dispatcher().Dispatch(dctx, ToolInvocation{
				CallID:     tc.ID,
				Name:       tc.Name,
				Args:       args,
				AppID:      in.AppID,
				AgentID:    agentID,
				AgentRunID: agentRunID(in.AgentRunID, agentID),
				UserID:     in.UserID,
				SessionID:  in.SessionID,
				UserJWT:    in.UserJWT,
			})
			if stopTicker != nil {
				close(stopTicker)
			}
			if toolCancel != nil {
				timedOut := dctx.Err() == context.DeadlineExceeded && ctx.Err() == nil
				toolCancel()
				if timedOut && outcomes[i].Status != "errored" {
					outcomes[i] = ToolOutcome{
						Status: "errored",
						Error: fmt.Sprintf(
							"tool %q exceeded the %s per-call time limit and was cancelled; narrow the operation (smaller scope / fewer files) or run it via background_run",
							tc.Name, e.ToolTimeout),
						DurationMs: time.Since(start).Milliseconds(),
					}
				}
			}
			if outcomes[i].DurationMs == 0 {
				outcomes[i].DurationMs = time.Since(start).Milliseconds()
			}
			if outcomes[i].Status == "" {
				if outcomes[i].Error != "" {
					outcomes[i].Status = "errored"
				} else {
					outcomes[i].Status = "completed"
				}
			}
			resultMap := outcomeToResult(outcomes[i])
			res := e.fireHook(ctx, in.AppID, agent, schema.HookEventToolEnd, withTurnState(hooks.Payload{
				AppID: in.AppID, SessionID: in.SessionID, UserID: in.UserID,
				TurnID: tr.ID, ToolName: tc.Name, ToolArgs: tc.Arguments,
				ToolStatus: outcomes[i].Status, ToolError: outcomes[i].Error,
				ToolResult: resultMap,
			}, metrics))
			if res.Modified {
				applyResultMutation(&outcomes[i], resultMap)
			}
			if len(res.Injects) > 0 {
				e.applyInjections(ctx, in, tr, res.Injects)
			}
			if e.ContextIncrement != nil {
				delta := 0
				for _, p := range outcomes[i].Parts {
					delta += len(p.Text)
				}
				if delta > 0 {
					e.ContextIncrement(in.SessionID, delta/4)
				}
			}
			if fact := AutoFact(tc.Name, tc.Arguments, outcomes[i].Status); fact != "" && e.Sessions != nil {
				_, _ = e.Sessions.AppendDurable(ctx, sessionstore.Event{
					Type:      sessionstore.EventMemoryFactAdded,
					SessionID: in.SessionID,
					AppID:     in.AppID,
					UserID:    in.UserID,
					Memory:    &sessionstore.MemoryPayload{Fact: fact},
				})
			}
		}(i, tc)
	}
	wg.Wait()
	return outcomes
}

func (e *Engine) runBehaviorClassifier(
	ctx context.Context, tr *turn.Turn, app *appmgr.RuntimeApp, agent *schema.Agent,
	be *behavior.Engine, in TurnInput, snap *sessionstore.SessionSnapshot,
	tools []llm.ToolSpec, apiKey, baseURL string,
) {
	cctx := ctx
	if to := be.ClassifierTimeout(); to > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, time.Duration(to)*time.Second)
		defer cancel()
	}
	chat := func(c context.Context, system, user string) (string, error) {
		resp, err := e.LLM.Chat(c, &llm.ChatRequest{
			BYOK:     app.Meta != nil && app.Meta.BYOK,
			Provider: agent.Brain.Provider,
			Model:    agent.Brain.Model,
			APIKey:   apiKey,
			BaseURL:  baseURL,
			UserJWT:  in.UserJWT,
			Messages: []llm.ChatMessage{
				{Role: "system", Content: system},
				{Role: "user", Content: user},
			},
			// Gateway attribution — system call, still billed to the
			// app/session/user that triggered it.
			AppID:         in.AppID,
			SessionID:     in.SessionID,
			UserID:        in.UserID,
			AgentID:       agentRunID(in.AgentRunID, agent.ID),
			CorrelationID: "classify:" + tr.ID,
		})
		if err != nil || resp == nil {
			return "", err
		}
		return resp.Content, nil
	}
	directive := be.Classify(cctx, in.SessionID, classifyInputFromSnap(*snap, tools), chat)
	if directive == "" {
		return
	}
	e.injectSystemDirective(ctx, in, tr.ID, directive, DirectiveBehaviorClassify, nil, nil)
	if st, err := e.Sessions.State(in.SessionID); err == nil && st != nil {
		*snap = st.Snapshot()
	}
}

func classifyInputFromSnap(snap sessionstore.SessionSnapshot, tools []llm.ToolSpec) behavior.ClassifyInput {
	in := behavior.ClassifyInput{}
	for i := len(snap.Messages) - 1; i >= 0; i-- {
		if snap.Messages[i].Role == "user" {
			in.UserMessage = snap.Messages[i].Content
			break
		}
	}
	for _, t := range tools {
		in.ToolInventory = append(in.ToolInventory, behavior.ToolInfo{
			Name: t.Name, Description: t.Description,
		})
	}
	for _, m := range snap.Messages {
		in.Recent = append(in.Recent, behavior.HistMsg{Role: m.Role, Content: m.Content})
	}
	return in
}

func (e *Engine) injectBehaviorNotes(ctx context.Context, in TurnInput, tr *turn.Turn, notes []behavior.Violation) {
	if len(notes) == 0 {
		return
	}
	parts := make([]string, 0, len(notes))
	severity := "warning"
	for _, v := range notes {
		parts = append(parts, v.Format())
		if v.Level == "block" {
			severity = "critical"
		}
	}
	body := wrapRuntimeDirective("behavior_enforcement", severity, strings.Join(parts, "\n\n"))
	e.injectSystemDirective(ctx, in, tr.ID, body, DirectiveBehaviorEnforce, nil, nil)
}

func (e *Engine) emitSecurityDecision(
	ctx context.Context,
	sessionID, appID, userID, correlationID string,
	agent *schema.Agent, module, action string,
	params map[string]any, d policy.Decision,
) {
	gate := string(d.Gate)
	if gate == "system_module_bypass" || gate == "meta_tool_bypass" {
		return
	}
	agentIDStr := ""
	if agent != nil {
		agentIDStr = agent.ID
	}
	riskLevel := ""
	if e.PolicyEvaluator != nil {
		if dp, ok := e.PolicyEvaluator.(*DefaultPolicyEvaluator); ok && dp.Lookup != nil {
			if spec := dp.Lookup.LookupToolSpec(module, action); spec != nil {
				riskLevel = string(spec.RiskLevel)
			}
		}
	}
	ev := sessionstore.Event{
		Type:          sessionstore.EventSecurityDecision,
		SessionID:     sessionID,
		AppID:         appID,
		UserID:        userID,
		CorrelationID: correlationID,
		Security: &sessionstore.SecurityDecisionPayload{
			AppID:          appID,
			AgentID:        agentIDStr,
			SessionID:      sessionID,
			UserID:         userID,
			Module:         module,
			Action:         action,
			RiskLevel:      riskLevel,
			ParamsRedacted: policy.RedactParams(params, nil),
			Decision:       d.Kind.String(),
			Gate:           gate,
			Reason:         d.Reason,
			Caller:         policy.CallerLLM.String(),
		},
	}
	if _, err := e.Sessions.AppendDurable(ctx, ev); err != nil {
		e.Logger.Warn("runtime: failed to emit security_decision audit row",
			slog.String("module", module),
			slog.String("action", action),
			slog.String("decision", d.Kind.String()),
			slog.String("err", err.Error()))
	}
}

func (e *Engine) awaitApproval(
	ctx context.Context,
	sessionID, appID, userID, correlationID string,
	app *appmgr.RuntimeApp, agent *schema.Agent,
	toolName, callID, module, action string,
	params map[string]any, reason string,
	tr *turn.Turn, in *TurnInput,
) approval.Resolution {
	if e.ApprovalRegistry == nil {
		return approval.Resolution{
			Result: approval.ResultCancelled,
			Reason: "no approval registry wired (SG-5 disabled at runtime)",
		}
	}

	requestID := uuid.NewString()
	agentIDStr := ""
	if agent != nil {
		agentIDStr = agent.ID
	}
	riskLevel := ""
	if e.PolicyEvaluator != nil {
		if d, ok := e.PolicyEvaluator.(*DefaultPolicyEvaluator); ok && d.Lookup != nil {
			if spec := d.Lookup.LookupToolSpec(module, action); spec != nil {
				riskLevel = string(spec.RiskLevel)
			}
		}
	}

	pending := e.ApprovalRegistry.Arm(requestID)

	_, _ = e.Sessions.AppendDurable(ctx, sessionstore.Event{
		Type:          sessionstore.EventApprovalRequest,
		SessionID:     sessionID,
		AppID:         appID,
		UserID:        userID,
		CorrelationID: correlationID,
		Approval: &sessionstore.ApprovalPayload{
			ID:         requestID,
			Kind:       "tool_call",
			Status:     "pending",
			Reason:     reason,
			AgentID:    agentIDStr,
			ToolName:   toolName,
			ToolParams: params,
			RiskLevel:  riskLevel,
			CallID:     callID,
		},
	})

	if tr != nil && in != nil {
		arRes := e.fireHook(ctx, appID, agent, schema.HookEventApprovalRequest, hooks.Payload{
			AppID: appID, SessionID: sessionID, UserID: userID,
			TurnID: correlationID, ToolName: toolName, ToolArgs: params,
		})
		e.applyInjections(ctx, *in, tr, arRes.Injects)
		_ = tr.TransitionTo(ctx, turn.PhaseWaitingApproval)
	}

	timeout := approvalTimeout(app)
	stopTicker := make(chan struct{})
	go keepaliveTicker(ctx, stopTicker)
	res := pending.Wait(ctx, timeout)
	close(stopTicker)

	var resultEvent sessionstore.EventType
	resultStatus := "denied"
	switch res.Result {
	case approval.ResultApproved:
		resultEvent = sessionstore.EventApprovalGranted
		resultStatus = "granted"
	case approval.ResultApprovedAlways:
		resultEvent = sessionstore.EventApprovalGranted
		resultStatus = "granted"
		sig := toolSignature(module, action, params)
		e.addAllowedSig(sessionID, sig)
		_, _ = e.Sessions.AppendDurable(ctx, sessionstore.Event{
			Type:      sessionstore.EventToolAllowed,
			SessionID: sessionID,
			AppID:     appID,
			UserID:    userID,
			Allowed:   &sessionstore.AllowedToolPayload{Signature: sig},
		})
		if e.PathPolicies != nil {
			if pp, ok := e.PathPolicies.PathPolicyFor(appID, sessionID); ok && pp.HasWorkdir() {
				_ = projectsettings.Allow(pp.Root(), sig)
			}
		}
	case approval.ResultDenied:
		resultEvent = sessionstore.EventApprovalDenied
	case approval.ResultTimeout:
		resultEvent = sessionstore.EventApprovalDenied
		resultStatus = "auto_denied"
	case approval.ResultCancelled:
		resultEvent = sessionstore.EventApprovalDenied
		resultStatus = "cancelled"
	}
	if res.Result == approval.ResultTimeout || res.Result == approval.ResultCancelled {
		_, _ = e.Sessions.AppendDurable(ctx, sessionstore.Event{
			Type:          resultEvent,
			SessionID:     sessionID,
			AppID:         appID,
			UserID:        userID,
			CorrelationID: correlationID,
			Approval: &sessionstore.ApprovalPayload{
				ID:     requestID,
				Status: resultStatus,
				Reason: res.Reason,
			},
		})
	}

	if tr != nil {
		_ = tr.TransitionTo(ctx, turn.PhaseRunning)
	}

	return res
}

func (e *Engine) enforceGate(
	ctx context.Context,
	sessionID, appID, userID, correlationID string,
	app *appmgr.RuntimeApp, agent *schema.Agent,
	toolName, callID string, args map[string]any,
	tr *turn.Turn, in *TurnInput,
) *ToolOutcome {
	agentID := ""
	if agent != nil {
		agentID = agent.ID
	}
	toolName = e.resolveToolName(appID, agentID, toolName)
	if hm, ha := splitToolName(toolName); args != nil {
		if spec := e.toolSpec(hm, ha); spec != nil {
			if healToolArgs(spec, toolName, args) && e.Logger != nil {
				e.Logger.Debug("runtime: healed tool args to match schema", "tool", toolName)
			}
		}
	}
	if g := modeGuardFromCtx(ctx); g.blocks(toolname.Canonicalize(toolName)) {
		return &ToolOutcome{Status: "errored", Error: g.blockedError(toolName)}
	}
	if tr == nil && in == nil {
		if be := e.behaviorFor(app); be != nil {
			if v := be.BlockedSubTool(sessionID, toolName, args); v != nil {
				return &ToolOutcome{Status: "errored", Error: v.Format()}
			}
		}
	}
	if pp, ok := workdir.PathPolicyFromContext(ctx); ok {
		module, action := splitToolName(toolName)
		if keys := e.pathParamNames(module, action); len(keys) > 0 && !strings.HasPrefix(module, "mcp_") {
			if err := workdir.EnforceArgs(pp, args, keys...); err != nil {
				return &ToolOutcome{Status: "errored", Error: "denied by workdir policy: " + err.Error()}
			}
		}
	}
	if e.PolicyEvaluator == nil {
		return nil
	}
	module, action := splitToolName(toolName)
	if e.isToolAllowed(sessionID, module, action, args) {
		return nil
	}
	d := e.PolicyEvaluator.Evaluate(ctx, EvaluateInput{
		AppID:       appID,
		SessionID:   sessionID,
		UserID:      userID,
		Module:      module,
		Action:      action,
		App:         app,
		Agent:       agent,
		ProjectCaps: ProjectCapsFromContext(ctx),
	})
	e.emitSecurityDecision(ctx, sessionID, appID, userID, correlationID, agent, module, action, args, d)
	switch d.Kind {
	case policy.DecisionDeny:
		return &ToolOutcome{
			Status: "errored",
			Error:  "denied by security policy (" + string(d.Gate) + "): " + d.Reason,
		}
	case policy.DecisionNeedsApproval:
		res := e.awaitApproval(ctx, sessionID, appID, userID, correlationID, app, agent, toolName, callID, module, action, args, d.Reason, tr, in)
		switch res.Result {
		case approval.ResultApproved, approval.ResultApprovedAlways:
			return nil
		case approval.ResultDenied:
			return &ToolOutcome{Status: "errored", Error: "approval denied: " + res.Reason}
		case approval.ResultTimeout:
			return &ToolOutcome{Status: "errored", Error: "approval timed out: " + res.Reason}
		case approval.ResultCancelled:
			return &ToolOutcome{Status: "errored", Error: "approval cancelled: " + res.Reason}
		}
	}
	return nil
}

// resolveToolName qualifies a bare tool name to its FQN via the dispatcher's
// resolver, so the gate evaluates the real tool instead of denying a bare
// action with an empty module. Unknown/ambiguous names are returned unchanged.
// ToolIndexFQNs returns the per-agent tool index's FQN list (empty agentID
// resolves to the app's default agent). Diagnostic surface for external tool
// testing — shows exactly what bare-name resolution can qualify against.
func (e *Engine) ToolIndexFQNs(ctx context.Context, appID, agentID string) []string {
	_, agent := e.resolveAppAgent(ctx, appID, agentID)
	aid := ""
	if agent != nil {
		aid = agent.ID
	}
	if r, ok := e.dispatcher().(interface{ IndexFQNs(appID, agentID string) []string }); ok {
		return r.IndexFQNs(appID, aid)
	}
	return nil
}

func (e *Engine) resolveToolName(appID, agentID, name string) string {
	if r, ok := e.dispatcher().(interface {
		ResolveToolName(appID, agentID, name string) string
	}); ok {
		return r.ResolveToolName(appID, agentID, name)
	}
	return name
}

func (e *Engine) pathParamNames(module, action string) []string {
	if spec := e.toolSpec(module, action); spec != nil {
		return spec.PathParamNames()
	}
	return nil
}

func (e *Engine) toolSpec(module, action string) *tool.Spec {
	if dp, ok := e.PolicyEvaluator.(*DefaultPolicyEvaluator); ok && dp.Lookup != nil {
		return dp.Lookup.LookupToolSpec(module, action)
	}
	return nil
}

func (e *Engine) GateSubTool(ctx context.Context, inv ToolInvocation) *ToolOutcome {
	app, agent := e.resolveAppAgent(ctx, inv.AppID, inv.AgentID)
	return e.enforceGate(ctx, inv.SessionID, inv.AppID, inv.UserID, inv.CallID, app, agent, inv.Name, inv.CallID, inv.Args, nil, nil)
}

func (e *Engine) ExecuteToolGated(ctx context.Context, inv ToolInvocation) ToolOutcome {
	if e.PathPolicies != nil {
		if pp, ok := e.PathPolicies.PathPolicyFor(inv.AppID, inv.SessionID); ok {
			ctx = workdir.WithPathPolicy(ctx, pp)
		}
	}
	// Normalise the invocation the way a turn would: default agent resolved,
	// bare/aliased tool name qualified ONCE — so the gate and the dispatcher
	// see the same canonical target (out-of-band callers rarely send either).
	if _, agent := e.resolveAppAgent(ctx, inv.AppID, inv.AgentID); agent != nil {
		inv.AgentID = agent.ID
	}
	inv.Name = e.resolveToolName(inv.AppID, inv.AgentID, inv.Name)
	if blocked := e.GateSubTool(ctx, inv); blocked != nil {
		return *blocked
	}
	return e.dispatcher().Dispatch(ctx, inv)
}

func (e *Engine) VoiceContext(ctx context.Context, appID, agentID string) (systemPrompt string, tools []llm.ToolSpec, err error) {
	app, _ := e.resolveAppAgent(ctx, appID, agentID)
	if app == nil || app.Definition == nil || len(app.Definition.Agents) == 0 {
		return "", nil, fmt.Errorf("runtime: voice context: app %q has no agent", appID)
	}
	agent := resolveAgent(app.Definition, agentID)
	if agent == nil {
		return "", nil, fmt.Errorf("runtime: voice context: agent %q not found in app %q", agentID, appID)
	}
	if e.Context == nil {
		sp := agent.SystemPrompt
		if sp == "" {
			sp = agent.Prompt
		}
		return sp, e.tools().ToolsForAgent(agent), nil
	}
	appName, appVersion := "", ""
	if app.Definition != nil {
		appName = app.Definition.App.Name
		appVersion = app.Definition.App.Version
	}
	if appName == "" && app.Meta != nil {
		appName = app.Meta.AppID
	}
	callAppEnabled, askUserWired, useSkillWired := false, false, false
	if pa, ok := e.Dispatcher.(primitiveAvailability); ok {
		callAppEnabled = pa.CallAppWired()
		askUserWired = pa.AskUserWired()
		useSkillWired = pa.UseSkillWired()
	}
	res, err := e.Context.BuildFor(ctx, ContextRequest{
		App:             app,
		Agent:           agent,
		AppName:         appName,
		AppVersion:      appVersion,
		MemoryEnabled:   appMemoryEnabled(app),
		AgentEnabled:    appAgentSpawnEnabled(app),
		CallAppEnabled:  callAppEnabled,
		AskUserEnabled:  askUserWired && appGrantsAskUser(app),
		UseSkillEnabled: useSkillWired && appHasSkills(app, agent),
	})
	if err != nil {
		return "", nil, err
	}
	return res.SystemPrompt, res.Tools, nil
}

func (e *Engine) resolveAppAgent(ctx context.Context, appID, agentID string) (*appmgr.RuntimeApp, *schema.Agent) {
	if e.Apps == nil {
		return nil, nil
	}
	app, err := e.Apps.Get(ctx, appID)
	if err != nil || app == nil || app.Definition == nil {
		return app, nil
	}
	// resolveAgent handles the empty-id default (entry agent → first agent) —
	// external callers (voice, tool exec API) rarely name an agent explicitly.
	return app, resolveAgent(app.Definition, agentID)
}

func approvalTimeout(app *appmgr.RuntimeApp) time.Duration {
	const defaultS = 3600
	const minS = 30
	const maxS = 3600
	v := defaultS
	if app != nil && app.Definition != nil && app.Definition.Tools != nil &&
		app.Definition.Tools.Capabilities != nil {
		if t := app.Definition.Tools.Capabilities.ApprovalTimeout; t > 0 {
			v = t
		}
	}
	if v < minS {
		v = minS
	}
	if v > maxS {
		v = maxS
	}
	return time.Duration(v) * time.Second
}

func splitToolName(name string) (module, action string) {
	name = toolname.Canonicalize(name)
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			return name[:i], name[i+1:]
		}
	}
	return "", name
}

func (e *Engine) persistToolResults(
	ctx context.Context, tr *turn.Turn, in TurnInput,
	calls []llm.ChatToolCall, outcomes []ToolOutcome,
) error {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	evs := make([]sessionstore.Event, len(calls))
	for i, tc := range calls {
		out := outcomes[i]
		var output any
		if len(out.Parts) == 0 {
			output = ""
		}
		tp := &sessionstore.ToolPayload{
			CallID:     tc.ID,
			Name:       tc.Name,
			Arguments:  maps.Clone(tc.Arguments),
			Status:     out.Status,
			Parts:      out.Parts,
			Output:     output,
			Error:      out.Error,
			DurationMs: out.DurationMs,
			Metadata:   maps.Clone(out.Metadata),
		}
		if d := out.Diff; d != nil {
			tp.Diff = d.Summary
			tp.UnifiedDiff = d.Unified
			tp.PreviousContent = d.PreviousContent
			tp.NewContent = d.NewContent
			if d.Additions > 0 || d.Deletions > 0 {
				if tp.Metadata == nil {
					tp.Metadata = map[string]any{}
				}
				tp.Metadata["additions"] = d.Additions
				tp.Metadata["deletions"] = d.Deletions
			}
		}
		evs[i] = sessionstore.Event{
			Type:          sessionstore.EventToolResult,
			SessionID:     in.SessionID,
			AppID:         in.AppID,
			UserID:        in.UserID,
			CorrelationID: tr.ID,
			StepID:        tr.StepID,
			Tool:          tp,
		}
	}

	if len(evs) > 1 {
		if batcher, ok := e.Sessions.(durableBatcher); ok {
			if _, err := batcher.AppendDurableBatch(ctx, evs); err != nil {
				return fmt.Errorf("runtime: persist %d tool_results: %w", len(evs), err)
			}
			e.touchContext(in.SessionID)
			return nil
		}
	}
	for i := range evs {
		if _, err := e.Sessions.AppendDurable(ctx, evs[i]); err != nil {
			return fmt.Errorf("runtime: persist tool_result %q: %w", calls[i].ID, err)
		}
	}
	e.touchContext(in.SessionID)
	return nil
}

type durableBatcher interface {
	AppendDurableBatch(ctx context.Context, evs []sessionstore.Event) ([]uint64, error)
}

func (e *Engine) dispatcher() ToolDispatcher {
	if e.Dispatcher == nil {
		return NoopDispatcher{}
	}
	return e.Dispatcher
}

func outcomeToResult(o ToolOutcome) map[string]any {
	if o.Status == "" && len(o.Parts) == 0 && o.Error == "" {
		return nil
	}
	var text string
	for _, p := range o.Parts {
		text += p.Text
	}
	return map[string]any{
		"status": o.Status,
		"error":  o.Error,
		"text":   text,
	}
}

func applyResultMutation(out *ToolOutcome, result map[string]any) {
	if out == nil || result == nil {
		return
	}
	if s, ok := result["status"].(string); ok && s != "" {
		out.Status = s
	}
	if e, ok := result["error"].(string); ok {
		out.Error = e
	}
	if txt, ok := result["text"].(string); ok {
		out.Parts = []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeText, Text: txt},
		}
	}
}

func (e *Engine) computeHookMetrics(snap sessionstore.SessionSnapshot, agent *schema.Agent, llmContent string, toolCallsUsed int) hooks.Payload {
	var turns int
	var lastUser string
	for i := range snap.Messages {
		if snap.Messages[i].Role == "user" {
			turns++
			if snap.Messages[i].Content != "" {
				lastUser = snap.Messages[i].Content
			}
		}
	}
	var cs contextsvc.Snapshot
	if agent != nil {
		cs = contextsvc.Resolve(snap, agent.Brain)
	} else {
		cs = contextsvc.Snapshot{TokensUsed: snap.ContextTokens}
	}
	used, maxTok := cs.TokensUsed, cs.MaxTokens
	if e != nil && agent != nil {
		if cv := e.freshContextView(snap.SessionID, snap, agent.Brain); cv.Limit > 0 {
			used, maxTok = cv.Used, cv.Limit
		}
	}
	openTasks, tasksSummary := openTaskState(snap.Todos)
	return hooks.Payload{
		MessageCount:  len(snap.Messages),
		TurnCount:     turns,
		ToolCallsUsed: toolCallsUsed,
		LLMContent:    llmContent,
		UserMessage:   lastUser,
		TokensUsed:    used,
		MaxTokens:     maxTok,
		OpenTasks:     openTasks,
		TasksSummary:  tasksSummary,
	}
}

func openTaskState(todos []sessionstore.Todo) (int, string) {
	var open []string
	for _, t := range todos {
		switch t.Status {
		case "", "pending", "in_progress":
			status := t.Status
			if status == "" {
				status = "pending"
			}
			id := t.ID
			if id == "" {
				id = "?"
			}
			open = append(open, fmt.Sprintf("%s (%s)", id, status))
		}
	}
	return len(open), strings.Join(open, ", ")
}

func withTurnState(p, m hooks.Payload) hooks.Payload {
	p.MessageCount = m.MessageCount
	p.TurnCount = m.TurnCount
	p.ToolCallsUsed = m.ToolCallsUsed
	p.LLMContent = m.LLMContent
	p.UserMessage = m.UserMessage
	p.TokensUsed = m.TokensUsed
	p.MaxTokens = m.MaxTokens
	p.OpenTasks = m.OpenTasks
	p.TasksSummary = m.TasksSummary
	return p
}

func (e *Engine) applyInjections(ctx context.Context, in TurnInput, tr *turn.Turn, injs []*hooks.MessageInjection) {
	for _, inj := range injs {
		e.applyInjection(ctx, in, tr, inj)
	}
}

func (e *Engine) applyInjection(ctx context.Context, in TurnInput, tr *turn.Turn, inj *hooks.MessageInjection) {
	if inj == nil || inj.Content == "" {
		return
	}
	role := inj.Role
	if role == "" {
		role = "user"
	}
	if role == "system" {
		e.injectSystemDirective(ctx, in, tr.ID, inj.Content, DirectiveHookInject, nil, nil)
		return
	}
	ev := sessionstore.Event{
		Type:          sessionstore.EventUserMessage,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: tr.ID,
		Message: &sessionstore.MessagePayload{
			Role:    role,
			Content: inj.Content,
		},
	}
	if _, err := e.Sessions.AppendDurable(ctx, ev); err != nil {
		e.Logger.Warn("runtime: hook inject_message append failed",
			slog.String("err", err.Error()))
	}
}

func isCancellation(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if ctx != nil {
		if cerr := ctx.Err(); cerr != nil {
			return true
		}
	}
	return false
}

func (e *Engine) runFlow(ctx context.Context, app *appmgr.RuntimeApp, in TurnInput) (*TurnResult, error) {
	flowCfg := app.Definition.Flow
	deps := flow.Deps{
		Sessions: e.Sessions,
		RunAgent: func(ctx context.Context, spec flow.AgentSpec) (flow.AgentResult, error) {
			r, err := e.RunSubAgent(ctx, SubAgentSpec{
				AppID:         spec.AppID,
				ParentSession: spec.ParentSession,
				UserID:        spec.UserID,
				UserJWT:       spec.UserJWT,
				AgentID:       spec.AgentID,
				RunID:         spec.RunID,
				Task:          spec.Task,
				MemorySeed:    spec.MemorySeed,
			})
			return flow.AgentResult{Status: r.Status, Content: r.Content, Error: r.Error}, err
		},
		RunTool: func(ctx context.Context, inv flow.ToolInvocation) flow.ToolOutcome {
			return e.flowToolHooked(ctx, inv)
		},
		Approvals: e.ApprovalRegistry,
		Logger:    e.Logger,
	}
	flowID := flowCfg.ID
	if flowID == "" {
		flowID = "flow"
	}
	tr, err := turn.New(turn.Options{
		SessionID: in.SessionID, AppID: in.AppID, AgentID: flowID,
		UserID: in.UserID, UserJWT: in.UserJWT,
		Pool: e.Pool, Sink: e.Sessions, IDGen: e.IDGen, Logger: e.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: new flow turn: %w", err)
	}
	if err := tr.Start(ctx); err != nil {
		return nil, fmt.Errorf("runtime: start flow turn: %w", err)
	}
	_ = tr.TransitionTo(ctx, turn.PhaseLoading)
	_ = tr.TransitionTo(ctx, turn.PhaseRunning)

	fr := flow.New(flowCfg, deps, e.IDGen)
	runIn := flow.RunInput(in.AppID, in.SessionID, in.UserID, in.UserJWT, tr.ID).
		WithEvent(e.flowEvent(in.SessionID))
	if e.AppSecrets != nil {
		lookup := e.AppSecrets
		appID := in.AppID
		runIn = runIn.WithSecretLookup(func(key string) (string, bool) {
			return lookup.Get(appID, key)
		})
	}
	res, runErr := fr.Run(ctx, flowCfg, runIn)
	if runErr != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cErr := tr.Fail(closeCtx, runErr); cErr != nil {
			e.Logger.Warn("runtime: flow turn fail emit error", slog.String("err", cErr.Error()))
		}
		e.emitTurnError(closeCtx, in, tr, runErr)
		return nil, runErr
	}

	_ = tr.TransitionTo(ctx, turn.PhasePersisting)
	seq, perr := e.persistAssistantStep(ctx, tr, in, &llm.ChatResponse{Content: res.Content})
	if perr != nil {
		e.Logger.Warn("runtime: persist flow result failed", slog.String("err", perr.Error()))
	}
	if err := tr.CloseDone(ctx); err != nil {
		e.Logger.Warn("runtime: flow turn close error", slog.String("err", err.Error()))
	}
	return &TurnResult{Seq: seq, Content: res.Content, TurnID: tr.ID}, nil
}

// flowEvent seeds the flow's `event.*` namespace from the triggering user
// turn. When a non-human launcher (background webhook) attached a structured
// TriggerEvent to the user message, that scope is used so nodes can read
// {{event.payload.id}} etc. Otherwise falls back to the message text only.
func (e *Engine) flowEvent(sessionID string) map[string]any {
	msg := ""
	var trigger map[string]any
	if st, err := e.Sessions.State(sessionID); err == nil && st != nil {
		snap := st.Snapshot()
		for i := len(snap.Messages) - 1; i >= 0; i-- {
			if snap.Messages[i].Role != "user" {
				continue
			}
			msg = snap.Messages[i].Content
			if msg == "" {
				for _, p := range snap.Messages[i].Parts {
					if p.Type == sessionstore.PartTypeText {
						msg += p.Text
					}
				}
			}
			trigger = snap.Messages[i].TriggerEvent
			break
		}
	}
	if len(trigger) > 0 {
		out := make(map[string]any, len(trigger)+2)
		for k, v := range trigger {
			out[k] = v
		}
		payload, _ := out["payload"].(map[string]any)
		if payload == nil {
			payload = map[string]any{}
			out["payload"] = payload
		}
		if msg != "" {
			if _, ok := payload["message"]; !ok {
				payload["message"] = msg
			}
			if _, ok := out["message"]; !ok {
				out["message"] = msg
			}
			if _, ok := out["text"]; !ok {
				out["text"] = msg
			}
		}
		return out
	}
	return map[string]any{
		"text":    msg,
		"message": msg,
		"payload": map[string]any{"message": msg, "text": msg},
	}
}

// flowToolHooked dispatches a flow tool node through the same tool_start /
// tool_end hook machinery agent-driven tool calls use, so a tool invoked by a
// flow behaves identically to one invoked by an agent (doc: "Tool hooks fired
// around node execution"). Flow tool nodes are agent-less, so hooks fire at
// app level (agent=nil); injection-style effects that need an LLM turn do not
// apply here, but gate (tool_start) and transform_result (tool_end) do.
func (e *Engine) flowToolHooked(ctx context.Context, inv flow.ToolInvocation) flow.ToolOutcome {
	payload := hooks.Payload{
		AppID: inv.AppID, SessionID: inv.SessionID, UserID: inv.UserID,
		ToolName: inv.Name, ToolArgs: inv.Args,
	}
	if gate := e.fireHookGate(ctx, inv.AppID, nil, schema.HookEventToolStart, payload); gate != nil && !gate.Allow {
		return flow.ToolOutcome{Status: "errored", Error: "blocked by hook gate: " + gate.Reason}
	}

	out := e.ExecuteToolGated(ctx, ToolInvocation{
		CallID:    inv.CallID,
		Name:      inv.Name,
		Args:      inv.Args,
		AppID:     inv.AppID,
		UserID:    inv.UserID,
		SessionID: inv.SessionID,
		UserJWT:   inv.UserJWT,
	})

	resultMap := outcomeToResult(out)
	endPayload := payload
	endPayload.ToolStatus = out.Status
	endPayload.ToolError = out.Error
	endPayload.ToolResult = resultMap
	if res := e.fireHook(ctx, inv.AppID, nil, schema.HookEventToolEnd, endPayload); res.Modified {
		applyResultMutation(&out, resultMap)
	}

	return flow.ToolOutcome{Status: out.Status, Parts: out.Parts, Error: out.Error}
}

func extractEmbeddedAuth(b schema.Brain) (apiKey, baseURL string) {
	if s, ok := b.Config["api_key"].(string); ok && s != "" {
		apiKey = s
	}
	if apiKey == "" {
		if s, ok := b.Credential.(string); ok && s != "" {
			apiKey = s
		} else if m, ok := b.Credential.(map[string]any); ok {
			if s, ok := m["api_key"].(string); ok && s != "" {
				apiKey = s
			}
		}
	}
	if s, ok := b.Config["base_url"].(string); ok && s != "" {
		baseURL = s
	}
	return apiKey, baseURL
}
