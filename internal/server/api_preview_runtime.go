package server

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/digitornai/digitorn/internal/modules/preview"
)

// The previewed app talks to the daemon through this one endpoint.
//
// The page is SERVED by the daemon, so it is same-origin with it and can call
// back without CORS, without a socket, and without the web client relaying
// anything — which is what keeps this feature from touching any code path that
// already works.
//
// One round trip does both directions: the page posts what it currently is, and
// the response carries whatever the agent asked it to do. Idle previews cost one
// small POST per interval; there is no second channel to keep alive.
//
// Isolation is not a check bolted on here — it is the shape of the call. The
// app and session come from the URL, the `?t=` token is an HMAC over exactly
// that pair, and the store is keyed by the same pair. A page holding session A's
// token cannot mint session B's token, so it cannot read or write B's state.
type previewRuntimeRequest struct {
	// For is the id of the command this report answers, empty for a spontaneous
	// report (first paint, a fresh runtime error, the idle heartbeat).
	For      string             `json:"for"`
	Snapshot previewSnapshotDTO `json:"snapshot"`
}

type previewSnapshotDTO struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	Ready    bool   `json:"ready"`
	Blank    bool   `json:"blank"`
	Text     string `json:"text"`
	Elements []struct {
		Ref   string `json:"ref"`
		Role  string `json:"role"`
		Text  string `json:"text"`
		Level int    `json:"level"`
		Name  string `json:"name"`
		Value string `json:"value"`
		Href  string `json:"href"`
	} `json:"elements"`
	Errors []struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
		Source  string `json:"source"`
		Line    int    `json:"line"`
		Column  int    `json:"column"`
		Stack   string `json:"stack"`
	} `json:"errors"`
	Failed []struct {
		Method string `json:"method"`
		URL    string `json:"url"`
		Status int    `json:"status"`
		Error  string `json:"error"`
	} `json:"failed_requests"`
	Logs []struct {
		Level string `json:"level"`
		Text  string `json:"text"`
	} `json:"logs"`
	Viewport string `json:"viewport"`
	Layout   *struct {
		OverflowX   int      `json:"overflow_x"`
		TinyText    int      `json:"tiny_text"`
		LowContrast int      `json:"low_contrast"`
		Samples     []string `json:"samples"`
	} `json:"layout"`
	Storage map[string]string `json:"storage"`
	Detail  *struct {
		Ref      string            `json:"ref"`
		Tag      string            `json:"tag"`
		Attrs    map[string]string `json:"attrs"`
		Styles   map[string]string `json:"styles"`
		Rect     string            `json:"rect"`
		Covered  bool              `json:"covered"`
		Disabled bool              `json:"disabled"`
		HTML     string            `json:"html"`
	} `json:"detail"`
}

// Caps applied to whatever the page sends. The page is the agent's own output
// and may be broken or hostile-by-accident (an infinite render loop spamming
// console.error), so the daemon truncates rather than trusts.
const (
	previewMaxText     = 20000
	previewMaxElements = 300
	previewMaxErrors   = 25
	previewMaxRequests = 25
	previewMaxLogs     = 40
	previewMaxField    = 2000
)

func (d *Daemon) postPreviewRuntime(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	sessionID := chi.URLParam(r, "session_id")
	if !d.checkPreviewToken(appID, sessionID, r.URL.Query().Get("t")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req previewRuntimeRequest
	if err := readJSON(r, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	snap := toPreviewSnapshot(req.Snapshot)
	store := preview.Shared()
	if id := strings.TrimSpace(req.For); id != "" {
		store.Complete(appID, sessionID, id, snap)
	} else {
		store.Report(appID, sessionID, snap)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"commands": store.Take(appID, sessionID),
	})
}

func toPreviewSnapshot(in previewSnapshotDTO) preview.Snapshot {
	out := preview.Snapshot{
		URL:   clip(in.URL, previewMaxField),
		Title: clip(in.Title, previewMaxField),
		Ready: in.Ready,
		Blank: in.Blank,
		Text:  clip(in.Text, previewMaxText),
	}
	for i, e := range in.Elements {
		if i >= previewMaxElements {
			break
		}
		out.Elements = append(out.Elements, preview.Element{
			Ref:   clip(e.Ref, 32),
			Role:  clip(e.Role, 32),
			Text:  clip(e.Text, 200),
			Level: e.Level,
			Name:  clip(e.Name, 120),
			Value: clip(e.Value, 200),
			Href:  clip(e.Href, 500),
		})
	}
	for i, e := range in.Errors {
		if i >= previewMaxErrors {
			break
		}
		out.Errors = append(out.Errors, preview.RuntimeError{
			Kind:    clip(e.Kind, 32),
			Message: clip(e.Message, previewMaxField),
			Source:  clip(e.Source, 500),
			Line:    e.Line,
			Column:  e.Column,
			Stack:   clip(e.Stack, 4000),
		})
	}
	for i, q := range in.Failed {
		if i >= previewMaxRequests {
			break
		}
		out.Failed = append(out.Failed, preview.Request{
			Method: clip(q.Method, 12),
			URL:    clip(q.URL, 500),
			Status: q.Status,
			Error:  clip(q.Error, 300),
		})
	}
	for i, l := range in.Logs {
		if i >= previewMaxLogs {
			break
		}
		out.Logs = append(out.Logs, preview.LogLine{
			Level: clip(l.Level, 12),
			Text:  clip(l.Text, 500),
		})
	}
	out.Viewport = clip(in.Viewport, 24)
	if l := in.Layout; l != nil {
		lay := preview.Layout{OverflowX: l.OverflowX, TinyText: l.TinyText, LowContrast: l.LowContrast}
		for i, sm := range l.Samples {
			if i >= 8 {
				break
			}
			lay.Samples = append(lay.Samples, clip(sm, 120))
		}
		out.Layout = &lay
	}
	if len(in.Storage) > 0 {
		out.Storage = map[string]string{}
		for k, v := range in.Storage {
			if len(out.Storage) >= 20 {
				break
			}
			out.Storage[clip(k, 80)] = clip(v, 300)
		}
	}
	if dt := in.Detail; dt != nil {
		d := preview.Detail{
			Ref: clip(dt.Ref, 32), Tag: clip(dt.Tag, 32), Rect: clip(dt.Rect, 80),
			Covered: dt.Covered, Disabled: dt.Disabled, HTML: clip(dt.HTML, 2000),
		}
		d.Attrs = clipMap(dt.Attrs, 20, 80, 300)
		d.Styles = clipMap(dt.Styles, 30, 40, 120)
		out.Detail = &d
	}
	return out
}

func clipMap(in map[string]string, maxKeys, keyLen, valLen int) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range in {
		if len(out) >= maxKeys {
			break
		}
		out[clip(k, keyLen)] = clip(v, valLen)
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
