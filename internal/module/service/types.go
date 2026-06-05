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
	"encoding/json"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
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
