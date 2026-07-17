package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/digitornai/digitorn/internal/core/servicebus"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/module/proxy"
)

type devInvokeRequest struct {
	ModuleID string          `json:"module_id"`
	Tool     string          `json:"tool"`
	Params   json.RawMessage `json:"params,omitempty"`
	UserID   string          `json:"user_id,omitempty"`
}

type devInvokeResponse struct {
	Success    bool   `json:"success"`
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Via        string `json:"via"`
}

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
		params = []byte(`{}`)
	}

	// Inject the EventBus into the context so modules that implement
	// EventEmitter can publish events.
	ctx := tool.WithEventBus(r.Context(), d.eventBus)

	// Optional identity so per-user credential injection (pieces auth) works
	// when driving an E2E check from outside a real session.
	if req.UserID != "" {
		ctx = tool.WithIdentity(ctx, tool.Identity{UserID: req.UserID, SessionID: "dev-e2e"})
	}

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
