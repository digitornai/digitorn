// Package service defines the gRPC contract that workers expose to host
// Digitorn modules. ONE service for ALL workerised modules — the daemon
// uses the same Invoke/Manifests path whether it talks to a worker
// hosting `shell`, `lsp`, `mcp`, etc.
//
// Why a single generic service ?
//
//   - Per-module gRPC contracts (à la internal/llm/service.go) work but
//     scale linearly with the number of workerised modules ; every new
//     heavy module would need its own .proto and its own client SDK.
//   - The runtime invokes modules through one interface : (module_id,
//     tool, params). The gRPC contract mirrors that interface. New
//     modules need ZERO contract change.
//
// Wire format : JSON (registered via codec.go). Versioned via ServiceName
// (digitorn.module.v1.ModuleService). Switch to protobuf is a v2 affair
// — the public Invoke/Manifests verbs stay the same.
package service

import (
	"context"
	"encoding/json"

	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
)

// InvokeRequest is the payload sent by the daemon to a worker. The
// caller MUST set ModuleID + ToolName ; everything else is optional.
type InvokeRequest struct {
	// ModuleID identifies the target module hosted by the worker. The
	// worker validates that it actually hosts this module ; an unknown
	// id yields tool.Result{Success: false, Error: "unknown module"}
	// (no Go error, so retries don't fire on a configuration bug).
	ModuleID string `json:"module_id"`

	// ToolName is the tool inside the module to invoke. Same error
	// semantics as ModuleID on miss.
	ToolName string `json:"tool_name"`

	// Params is the tool input, opaque to the framework. Modules
	// decode it themselves with their own json.Unmarshal into typed
	// param structs. An empty value is forwarded as `{}` so module
	// handlers always receive valid JSON.
	Params json.RawMessage `json:"params,omitempty"`

	// RequestID propagates a correlation ID across the daemon→worker
	// boundary for tracing. The worker echoes it back on the response
	// and into its access log. Free-form ; daemon usually fills it
	// with the gRPC request ID or a session-derived ulid.
	RequestID string `json:"request_id,omitempty"`

	// DeadlineMs is an advisory hint of how long the daemon will wait
	// before giving up. Workers may use it to short-circuit obviously
	// slow tools ; the HARD deadline still lives in the gRPC ctx.
	// 0 means "no hint" — fall back to the ctx deadline.
	DeadlineMs int64 `json:"deadline_ms,omitempty"`

	// Caller identity propagated across the worker boundary so worker-side
	// modules (and their audit) know who is calling. All optional ; an empty
	// value is an anonymous / system call. The worker re-injects these into
	// the request context before invoking the module.
	AppID     string `json:"app_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`

	// Config carries the calling app's resolved per-module config block
	// (tools.modules.<id>) as JSON, so a worker-hosted module reads its
	// app-specific configuration per call (a shared worker instance can't
	// take it at Init). Empty for modules/apps with no config block.
	Config json.RawMessage `json:"config,omitempty"`

	// AuthContext is a per-call, per-user resolved credential the daemon
	// injects for MCP tools (never cached in Config — Config is user-agnostic).
	// nil for non-MCP or unauthenticated calls. The worker re-injects it into
	// the call context; the module applies it as an http header or stdio env.
	AuthContext *AuthContext `json:"auth,omitempty"`
}

// AuthContext is a resolved credential bound to one (user, server) call. Token
// is already decrypted; the daemon resolves+refreshes it. EnvTokenVar names the
// stdio env var to inject under (empty for http, which uses an Authorization
// header). It rides the wire per-call and must never be logged verbatim.
type AuthContext struct {
	Token        string `json:"token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	EnvTokenVar  string `json:"env_token_var,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	Provider     string `json:"provider,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
}

// AuthChallenge describes a missing/expired credential. The daemon turns it into
// a blocking needs-auth result so the client can drive the OAuth flow. It carries
// no secrets.
type AuthChallenge struct {
	Provider string `json:"provider"`
	ServerID string `json:"server_id"`
	AuthURL  string `json:"auth_url"`
	State    string `json:"state"`
}

// AuthResolver resolves a per-call credential for an MCP tool. Implemented by the
// daemon's mcpoauth service. A nil *AuthContext with a nil *AuthChallenge means
// "no auth needed" (non-OAuth server); a non-nil *AuthChallenge means the call
// must be blocked pending authorization.
type AuthResolver interface {
	ResolveAuth(ctx context.Context, userID, appID, moduleID, toolName string) (*AuthContext, *AuthChallenge, error)
}

// InvokeResponse is what the worker returns. Result is always set ;
// DurationMs is best-effort observability.
type InvokeResponse struct {
	Result tool.Result `json:"result"`

	// RequestID echoes back the caller's RequestID so the daemon can
	// correlate the response without keeping per-call state.
	RequestID string `json:"request_id,omitempty"`

	// DurationMs is the wall-clock time the worker spent in the
	// handler (excluding gRPC marshal/unmarshal). For observability.
	DurationMs int64 `json:"duration_ms,omitempty"`
}

// ManifestsRequest has no fields ; reserved for future filters (e.g.
// "give me only modules X, Y"). Empty struct keeps the proto stable.
type ManifestsRequest struct{}

// ManifestsResponse is what the worker advertises : every module it
// hosts, with its manifest (tools, version, supported platforms).
// The daemon calls Manifests() once at boot to validate that the
// worker actually serves what its config promises.
type ManifestsResponse struct {
	// Modules is the manifests of every module the worker hosts,
	// sorted by ID for deterministic output. Empty list = the worker
	// is alive but hosts nothing (probably a configuration bug).
	Modules []domainmodule.Manifest `json:"modules"`

	// WorkerID is the framework-assigned id (kind#index) of this
	// worker instance. Returned for diagnostics — the daemon already
	// knows it from worker.Manager.Pool() so this is just a
	// belt-and-braces sanity check on the wire.
	WorkerID string `json:"worker_id,omitempty"`
}

// ToolsRequest asks a worker for a module's runtime tool set. Identity + Config
// ride along so a shared worker materializes the caller app's tools.
type ToolsRequest struct {
	ModuleID string          `json:"module_id"`
	AppID    string          `json:"app_id,omitempty"`
	AgentID  string          `json:"agent_id,omitempty"`
	UserID   string          `json:"user_id,omitempty"`
	Config   json.RawMessage `json:"config,omitempty"`

	// AuthContext is a per-user resolved credential the daemon injects so an
	// OAuth-gated MCP server can be CONNECTED at tool-listing time (not only at
	// invoke). Without it such a server returns 401 and lists no tools, so the
	// agent never sees them. nil for non-MCP / unauthenticated listings.
	AuthContext *AuthContext `json:"auth,omitempty"`
}

// ToolsResponse is the module's current tool specs (static manifest fallback for
// non-discovery modules).
type ToolsResponse struct {
	ModuleID string      `json:"module_id"`
	Tools    []tool.Spec `json:"tools"`
	WorkerID string      `json:"worker_id,omitempty"`
}
