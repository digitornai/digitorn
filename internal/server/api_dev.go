package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/mbathepaul/digitorn/internal/core/servicebus"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/module/proxy"
)

// devInvokeRequest is what POST /api/_dev/invoke accepts. Used by
// operators + integration tests to dispatch a tool call from outside
// the daemon, ahead of the runtime being fully wired. ONLY active when
// cfg.Auth.DevMode is true.
type devInvokeRequest struct {
	ModuleID string          `json:"module_id"`
	Tool     string          `json:"tool"`
	Params   json.RawMessage `json:"params,omitempty"`
}

// devInvokeResponse exposes the dispatched tool.Result PLUS metadata
// about where the call went : "in-proc" or "worker:<pool-id>". This
// makes it possible to verify from the outside that a configured
// worker pool is actually serving its modules.
type devInvokeResponse struct {
	Success    bool   `json:"success"`
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Via        string `json:"via"` // "in-proc" or "worker:<kind>"
}

// devInvoke implements POST /api/_dev/invoke. Validates the input,
// looks up the module in the servicebus to determine its execution
// path (in-proc module vs ProxyModule pointing at a worker), then
// dispatches via bus.Call and returns the typed response.
//
// Note : the route is gated to dev_mode at mount-time (routes.go),
// so production deployments never expose it.
func (d *Daemon) devInvoke(w http.ResponseWriter, r *http.Request) {
	var req devInvokeRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.ModuleID == "" || req.Tool == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "module_id and tool required")
		return
	}

	// Lookup the module to label its execution path (proxy vs
	// in-proc). The bus is the source of truth — same instance
	// the runtime will call, no second source of routing info to
	// keep in sync.
	via := "unknown"
	if sb, ok := d.bus.(*servicebus.Bus); ok {
		if mod, found := sb.Get(req.ModuleID); found {
			if p, isProxy := mod.(*proxy.ProxyModule); isProxy {
				via = "worker:" + string(p.Kind())
			} else {
				via = "in-proc"
			}
		}
	}

	params := []byte(req.Params)
	if len(params) == 0 {
		// servicebus.Bus.Call already tolerates nil ; we just keep
		// the wire shape stable for callers using `params: {}`.
		params = []byte(`{}`)
	}

	// Inject the EventBus into the context so modules that implement
	// EventEmitter can publish events.
	ctx := tool.WithEventBus(r.Context(), d.eventBus)

	start := time.Now()
	res, callErr := d.bus.Call(ctx, req.ModuleID, req.Tool, params)
	elapsed := time.Since(start)

	resp := devInvokeResponse{
		Success:    res.Success,
		Data:       res.Data,
		Error:      res.Error,
		DurationMs: elapsed.Milliseconds(),
		Via:        via,
	}
	// Surface the Go-level error if the Result didn't already carry
	// one (e.g. transport failure with empty Result.Error).
	if callErr != nil && resp.Error == "" {
		resp.Error = callErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}
