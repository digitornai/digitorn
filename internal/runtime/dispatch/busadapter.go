// Package dispatch wires the runtime's abstract ToolDispatcher to a
// concrete execution backend. The BusAdapter is the production
// implementation : it forwards every tool call to the in-process
// servicebus, where the daemon's modules are registered.
//
// The package sits inside internal/runtime to keep the dependency
// arrow pointing the right way : runtime owns the dispatcher
// contract, and the adapter is a runtime-side adapter onto a port.
// It never imports concrete modules — it talks only to the
// ports.ServiceBus interface so tests can swap in a fake.
package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/ports"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	pkgmodule "github.com/mbathepaul/digitorn/pkg/module"
)

// BlobPutter stores a tool's binary output (an image read, a generated file) in
// the content-addressed BlobStore and returns a reference. *blobstore.Store
// satisfies it. nil = binary tool output degrades to a text note (the model
// isn't shown the bytes, but is told they exist).
type BlobPutter interface {
	Put(ctx context.Context, mime string, r io.Reader) (sessionstore.BlobRef, error)
}

// BusAdapter bridges runtime.ToolDispatcher to a ports.ServiceBus.
// One BusAdapter is shared by every turn ; it carries no per-call
// state. Concurrency-safe because the underlying Bus is.
//
// Lifecycle :
//
//   - Construct with NewBusAdapter(bus). The bus must outlive the
//     adapter (it does : the daemon owns both).
//   - The adapter is plugged into Engine.Dispatcher AT BOOT, after
//     the MetaDispatcher (so the dispatch chain is
//     LLM → MetaDispatcher → BusAdapter → module).
//
// Cancellation : ctx is propagated verbatim to bus.Call. The
// module's Invoke MUST honour ctx so a turn timeout / abort
// doesn't leak module-side work.
type BusAdapter struct {
	Bus ports.ServiceBus

	// Pipelines resolves the per-app tool-call middleware onion (retry,
	// timeout, circuit_breaker, audit, dedup, semantic_cache, auto_heal,
	// cross_context, budget) for an (app, module) pair. nil — or a nil
	// pipeline for the pair — means the call runs straight through. The
	// onion runs daemon-side, so it wraps both in-process modules and the
	// gRPC ProxyModule round-trip identically.
	Pipelines PipelineSource

	// NowFn lets tests pin the clock for deterministic DurationMs
	// assertions. Production leaves it nil (defaults to time.Now).
	NowFn func() time.Time

	// Blobs stores binary tool output (image/file) so it reaches the LLM as a
	// real multipart Part. nil = binary output degrades to a text note.
	Blobs BlobPutter

	// FileChangeNotifier is attached to every dispatch ctx so a mutating module
	// (filesystem write/edit) can signal a workspace change for the live preview
	// push. nil = no live push wired (tests / headless). The notifier's
	// FileChanged is non-blocking by contract.
	FileChangeNotifier tool.FileChangeNotifier

	// ModuleConfigs resolves the calling app's per-module config block
	// (tools.modules.<id>) so it reaches the module at call time — the
	// only correct path for a shared (in-proc or worker) module instance.
	// nil = no per-app module config delivered (the historic behaviour).
	ModuleConfigs ModuleConfigSource

	// Embedder / Reranker are injected into every in-process dispatch ctx
	// so in-proc modules (e.g. filesystem's code-intelligence grep) reach
	// embeddings/rerank like worker modules do via the gateway. nil =
	// unavailable (module degrades gracefully).
	Embedder pkgmodule.Embedder
	Reranker pkgmodule.Reranker

	// EventBus is injected into every dispatch ctx so modules that implement
	// EventEmitter can publish events. nil = no bus wired (modules skip
	// event emission).
	EventBus ports.EventBus
}

// ModuleConfigSource resolves the per-app config block for a module.
// Returning nil/empty means "no config for this (app, module)".
type ModuleConfigSource interface {
	ModuleConfig(appID, moduleID string) map[string]any
}

// ToolPipeline wraps a terminal module call in the per-app onion. params is
// the marshalled tool input (dedup key / cache query) ; terminal performs the
// real Bus.Call. Implemented by *toolmw.Pipeline (structural — no import
// coupling).
type ToolPipeline interface {
	Run(ctx context.Context, params []byte, terminal func(context.Context) (tool.Result, error)) (tool.Result, error)
}

// PipelineSource resolves the onion for an (app, module) pair. Returning nil
// means "no middleware for this pair" — the fast path with zero overhead.
type PipelineSource interface {
	PipelineFor(appID, moduleID string) ToolPipeline
}

// legacyWorkspaceFileTools are the file operations the old `workspace` module
// exposed, now served by `filesystem`. The new `workspace` module's git tools
// (baseline/changes/diff/commit) are deliberately NOT in this set, so they keep
// routing to the workspace module.
var legacyWorkspaceFileTools = map[string]bool{
	"read": true, "write": true, "edit": true, "multi_edit": true,
	"glob": true, "grep": true, "delete": true,
}

// aliasLegacyToolModule redirects a legacy workspace file-op to filesystem at
// dispatch time. Mirrors the compile-time moduleAliases (compiler/aliases.go),
// but scoped per-tool so the internal git tools on the same module id are safe.
func aliasLegacyToolModule(moduleID, action string) string {
	if moduleID == "workspace" && legacyWorkspaceFileTools[action] {
		return "filesystem"
	}
	return moduleID
}

// aliasMCPTool routes mcp_<server>.<action> to the single mcp module as
// (mcp, mcp_<server>__<action>); the module re-derives server + tool. Gates
// already ran upstream with the mcp_<server> id.
func aliasMCPTool(moduleID, action string) (string, string) {
	if strings.HasPrefix(moduleID, "mcp_") {
		return "mcp", moduleID + "__" + action
	}
	return moduleID, action
}

// aliasPiecesTool routes ap_<piece>.<action> to the single pieces module as
// (pieces, ap_<piece>__<action>); the module re-derives piece + action.
func aliasPiecesTool(moduleID, action string) (string, string) {
	if strings.HasPrefix(moduleID, "ap_") {
		return "pieces", moduleID + "__" + action
	}
	return moduleID, action
}

// callBus runs the module call through the per-app onion when one is wired,
// else straight through. The terminal closure is what the innermost layer
// (or the no-onion fast path) ultimately invokes.
func (a *BusAdapter) callBus(ctx context.Context, appID, moduleID, actionName string, raw []byte) (tool.Result, error) {
	terminal := func(ctx context.Context) (tool.Result, error) {
		return a.Bus.Call(ctx, moduleID, actionName, raw)
	}
	if a.Pipelines != nil {
		if p := a.Pipelines.PipelineFor(appID, moduleID); p != nil {
			return p.Run(ctx, raw, terminal)
		}
	}
	return terminal(ctx)
}

// NewBusAdapter constructs an adapter over the given service bus.
// Returns nil when bus is nil — the caller must check, because a
// nil dispatcher would fall back to the runtime's NoopDispatcher,
// which is the documented "tool dispatcher not wired" behaviour.
func NewBusAdapter(bus ports.ServiceBus) *BusAdapter {
	if bus == nil {
		return nil
	}
	return &BusAdapter{Bus: bus}
}

// Dispatch implements runtime.ToolDispatcher. The contract :
//
//  1. call.Name MUST be a fully-qualified tool name of the form
//     "module.action" (dot separator). Names without a dot are
//     rejected with a clean error — the LLM sees the failure and
//     can recover. Names sanitized with "__" by the CB-3 layer
//     are unchanged here ; that layer must canonicalize before
//     calling us.
//
//  2. call.Args is marshalled to JSON and handed to the module.
//     A nil map encodes to "null" — modules that take no args
//     should accept that. We reject only un-encodable maps (NaN
//     floats, channels, funcs) with a clean error.
//
//  3. The Bus.Call result is converted to a ToolOutcome :
//     - Success=true  → Status="completed", a single Part carrying
//     the rendered data (text or JSON depending
//     on shape) plus an explicit ToolResult
//     spec for downstream UI/projection.
//     - Success=false → Status="errored", Error populated from the
//     module's error string.
//     - bus error     → Status="errored", Error wraps the bus error
//     (module not found, etc.).
//
//  4. DurationMs is always populated from the wall-clock between
//     entry and exit of this function.
func (a *BusAdapter) Dispatch(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	start := a.now()

	moduleID, actionName, ok := splitFQN(call.Name)
	if !ok {
		return errored(call.Name, "tool name must be of the form 'module.action'", start, a.now())
	}
	// Back-compat safety net: legacy `workspace.<file-op>` → `filesystem`, for
	// callers the compile-time alias doesn't rewrite (hook module_action, the
	// REST execute endpoint, an app installed before the alias existed). The new
	// workspace module's git tools are NOT file-ops, so they keep routing here.
	moduleID = aliasLegacyToolModule(moduleID, actionName)
	moduleID, actionName = aliasMCPTool(moduleID, actionName)
	moduleID, actionName = aliasPiecesTool(moduleID, actionName)

	raw, err := json.Marshal(call.Args)
	if err != nil {
		return errored(call.Name, "marshal args: "+err.Error(), start, a.now())
	}

	if a.Bus == nil {
		return errored(call.Name, "service bus is nil", start, a.now())
	}

	// Attach the caller identity so the tool-call middleware pipeline can
	// scope per-session state (dedup / cache) and key app-global state
	// (circuit breaker / budget), and so worker-side modules know the caller.
	ctx = tool.WithIdentity(ctx, tool.Identity{
		AppID: call.AppID, SessionID: call.SessionID, UserID: call.UserID,
		AgentID: call.AgentID, ModuleID: moduleID, ToolName: actionName,
	})

	// Deliver the app's per-module config so the module reads its
	// app-specific configuration on this call (in-proc reads it from ctx ;
	// the worker proxy forwards it across the boundary).
	if a.ModuleConfigs != nil {
		if cfg := a.ModuleConfigs.ModuleConfig(call.AppID, moduleID); len(cfg) > 0 {
			ctx = pkgmodule.WithModuleConfig(ctx, cfg)
		}
	}
	if a.Embedder != nil {
		ctx = pkgmodule.WithEmbedder(ctx, a.Embedder)
	}
	if a.Reranker != nil {
		ctx = pkgmodule.WithReranker(ctx, a.Reranker)
	}

	// Plumb the live workspace notifier so a mutating module can signal a file
	// change (filesystem emits directly — see notifyFileChange in the module).
	if a.FileChangeNotifier != nil {
		ctx = tool.WithFileChangeNotifier(ctx, a.FileChangeNotifier)
	}

	// Inject the EventBus so modules that implement EventEmitter can publish
	// events. nil = no bus wired (modules skip event emission).
	if a.EventBus != nil {
		ctx = tool.WithEventBus(ctx, a.EventBus)
	}

	res, busErr := a.callBus(ctx, call.AppID, moduleID, actionName, raw)
	end := a.now()

	if busErr != nil {
		// The bus may also have populated res.Error ; prefer the
		// returned error since it carries the structured wrap
		// (module not found, etc.).
		msg := busErr.Error()
		if res.Error != "" && !strings.Contains(msg, res.Error) {
			msg = msg + ": " + res.Error
		}
		return errored(call.Name, msg, start, end)
	}

	if !res.Success {
		errMsg := res.Error
		if errMsg == "" {
			errMsg = "tool reported failure without error message"
		}
		// A failed tool still carries output the LLM needs to understand WHY it
		// failed (a command's stderr, an exit code, a validation message). Send
		// that output as Parts alongside the error — without it the model only
		// sees "exit code 1" and is blind to the actual error.
		return runtime.ToolOutcome{
			Status:     "errored",
			Error:      "tool=" + call.Name + ": " + errMsg,
			Parts:      a.partsFromResult(ctx, res),
			DurationMs: end.Sub(start).Milliseconds(),
			Diff:       res.Diff,
			Metadata:   res.Metadata,
		}
	}

	return runtime.ToolOutcome{
		Status:     "completed",
		Parts:      a.partsFromResult(ctx, res), // LLM-visible text/image/file
		DurationMs: end.Sub(start).Milliseconds(),
		Diff:       res.Diff,     // client-only : never enters Parts
		Metadata:   res.Metadata, // client-only side-channel
	}
}

// splitFQN splits "module.action" into its two components. Returns
// false when there is no dot OR when either side is empty (e.g.
// ".read", "filesystem.", just "filesystem", just "."). Multi-dot
// names like "ns.mod.action" are split on the FIRST dot — modules
// can use dots in action names without breaking the contract.
func splitFQN(name string) (module, action string, ok bool) {
	dot := strings.IndexByte(name, '.')
	if dot <= 0 || dot == len(name)-1 {
		return "", "", false
	}
	return name[:dot], name[dot+1:], true
}

// errored is the canonical "this call failed" outcome shape.
// Always populates DurationMs so the audit row gets a real number.
func errored(toolName, msg string, start, end time.Time) runtime.ToolOutcome {
	return runtime.ToolOutcome{
		Status:     "errored",
		Error:      "tool=" + toolName + ": " + msg,
		DurationMs: end.Sub(start).Milliseconds(),
	}
}

// partsFromResult renders a tool.Result into the MessageParts the
// engine will persist on EventToolResult and re-feed to the next
// LLM turn. V1 keeps this simple : one Text part carrying the
// rendered Data. Strings pass through verbatim ; []byte is decoded
// as UTF-8 ; everything else is JSON-encoded with indent=2 so the
// LLM sees a clean structure.
//
// Multi-format (image blobs, audio) is intentionally NOT supported
// in V1 — modules return their content as JSON Data and the UI
// re-renders from that. Multimedia parts land via FT-* / RT-4.
// partsFromResult turns a tool result into the LLM-visible message parts.
// Without OutputParts it is the legacy single text part from Data. With
// OutputParts it builds rich parts : text passes through ; image/file/audio/
// video bytes are stored in the BlobStore and emitted as a binary Part the
// multipart adapter forwards to the model (so `read` of a PNG → the model sees
// it). A missing blob store, an empty payload, or a store error each degrade to
// a text note so the model is never silently blind.
func (a *BusAdapter) partsFromResult(ctx context.Context, res tool.Result) []sessionstore.MessagePart {
	if len(res.OutputParts) == 0 {
		return []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: renderData(res.Data)}}
	}
	textPart := func(s string) sessionstore.MessagePart {
		return sessionstore.MessagePart{Type: sessionstore.PartTypeText, Text: s}
	}
	out := make([]sessionstore.MessagePart, 0, len(res.OutputParts))
	for _, op := range res.OutputParts {
		switch op.Kind {
		case tool.OutputText, "":
			if op.Text != "" {
				out = append(out, textPart(op.Text))
			}
		case tool.OutputImage, tool.OutputAudio, tool.OutputVideo, tool.OutputFile:
			if a.Blobs == nil || len(op.Bytes) == 0 {
				out = append(out, textPart(fmt.Sprintf("[%s %q (%d bytes) — not viewable here]", op.Kind, op.Name, len(op.Bytes))))
				continue
			}
			ref, err := a.Blobs.Put(ctx, op.Mime, bytes.NewReader(op.Bytes))
			if err != nil {
				out = append(out, textPart(fmt.Sprintf("[%s %q — could not be stored: %v]", op.Kind, op.Name, err)))
				continue
			}
			out = append(out, sessionstore.MessagePart{Type: blobPartType(op.Kind), Blob: &ref})
		default:
			if op.Text != "" {
				out = append(out, textPart(op.Text))
			}
		}
	}
	if len(out) == 0 {
		out = append(out, textPart(renderData(res.Data)))
	}
	return out
}

func blobPartType(kind string) string {
	switch kind {
	case tool.OutputImage:
		return sessionstore.PartTypeImage
	case tool.OutputAudio:
		return sessionstore.PartTypeAudio
	case tool.OutputVideo:
		return sessionstore.PartTypeVideo
	default:
		return sessionstore.PartTypeFile
	}
}

// renderData turns res.Data into a string suitable for an LLM
// message. Strings/bytes pass through ; everything else gets
// JSON-encoded. errors during encoding fall back to fmt.Sprintf
// so we never panic on exotic types.
func renderData(data any) string {
	if data == nil {
		return ""
	}
	switch v := data.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", data)
	}
	return string(b)
}

func (a *BusAdapter) now() time.Time {
	if a == nil || a.NowFn == nil {
		return time.Now()
	}
	return a.NowFn()
}

// Compile-time guard : *BusAdapter must satisfy ToolDispatcher.
var _ runtime.ToolDispatcher = (*BusAdapter)(nil)
