package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/digitornai/digitorn/internal/modules/preview"
)

// The preview channel is the one place where code the agent WROTE talks back to
// the daemon. These tests pin the boundary: a page can only ever speak for the
// session it was opened for, and whatever it sends is truncated rather than
// trusted.

func previewTestDaemon() *Daemon {
	d := &Daemon{}
	d.previewSecretOnce.Do(func() {
		d.previewSecret = bytes.Repeat([]byte{7}, 32)
	})
	return d
}

func postRuntime(t *testing.T, d *Daemon, app, session, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost,
		"/api/apps/"+app+"/sessions/"+session+"/preview/runtime?t="+token,
		bytes.NewReader(raw))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("app_id", app)
	rctx.URLParams.Add("session_id", session)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	d.postPreviewRuntime(w, r)
	return w
}

func TestPreviewRuntimeRejectsAForeignToken(t *testing.T) {
	d := previewTestDaemon()
	tokenA := d.previewToken("app", "A")

	// The page for session A tries to report into session B using the only
	// token it has. It must be refused, and B must stay empty.
	w := postRuntime(t, d, "app", "B", tokenA, previewRuntimeRequest{})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — session A wrote into session B", w.Code)
	}
	if _, seen := preview.Shared().Observe("app", "B"); seen {
		t.Fatal("session B received state from a page holding session A's token")
	}
}

func TestPreviewRuntimeRejectsAMissingOrGarbageToken(t *testing.T) {
	d := previewTestDaemon()
	for _, tok := range []string{"", "deadbeef", d.previewToken("other-app", "A")} {
		if w := postRuntime(t, d, "app", "A", tok, previewRuntimeRequest{}); w.Code != http.StatusForbidden {
			t.Errorf("token %q got status %d, want 403", tok, w.Code)
		}
	}
}

func TestPreviewRuntimeStoresTheReportAndHandsBackCommands(t *testing.T) {
	d := previewTestDaemon()
	const app, session = "app", "runtime-happy"
	defer preview.Shared().Forget(app, session)

	body := map[string]any{
		"snapshot": map[string]any{
			"url":   "http://x/#/",
			"title": "Ma boutique",
			"ready": true,
			"text":  "Bienvenue",
			"elements": []map[string]any{
				{"ref": "e1", "role": "button", "text": "Panier"},
			},
			"errors": []map[string]any{
				{"kind": "error", "message": "products is not iterable", "line": 12},
			},
		},
	}
	w := postRuntime(t, d, app, session, d.previewToken(app, session), body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var resp struct {
		Commands []preview.Command `json:"commands"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Commands) != 0 {
		t.Errorf("an idle preview must not receive commands, got %+v", resp.Commands)
	}

	snap, seen := preview.Shared().Observe(app, session)
	if !seen {
		t.Fatal("the report never reached the store")
	}
	if snap.Title != "Ma boutique" || snap.Text != "Bienvenue" {
		t.Errorf("state lost in transit: %+v", snap)
	}
	if len(snap.Elements) != 1 || snap.Elements[0].Ref != "e1" {
		t.Errorf("elements lost: %+v", snap.Elements)
	}
	if len(snap.Errors) != 1 || snap.Errors[0].Message != "products is not iterable" {
		t.Fatalf("the runtime failure the agent needs is missing: %+v", snap.Errors)
	}
	if snap.Errors[0].Line != 12 {
		t.Errorf("line lost, the agent could not locate the crash")
	}
}

func TestPreviewRuntimeTruncatesWhatThePageSends(t *testing.T) {
	// The page is the agent's own output and can be broken — an infinite render
	// loop screaming into console.error, a giant DOM. The daemon must clamp it.
	d := previewTestDaemon()
	const app, session = "app", "runtime-flood"
	defer preview.Shared().Forget(app, session)

	huge := bytes.Repeat([]byte("a"), previewMaxText*3)
	elements := make([]map[string]any, 0, previewMaxElements*2)
	for i := 0; i < previewMaxElements*2; i++ {
		elements = append(elements, map[string]any{"ref": "e", "role": "button", "text": "x"})
	}
	errs := make([]map[string]any, 0, previewMaxErrors*3)
	for i := 0; i < previewMaxErrors*3; i++ {
		errs = append(errs, map[string]any{"kind": "console.error", "message": "loop", "line": i})
	}

	w := postRuntime(t, d, app, session, d.previewToken(app, session), map[string]any{
		"snapshot": map[string]any{
			"text": string(huge), "elements": elements, "errors": errs,
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	snap, _ := preview.Shared().Observe(app, session)
	if len(snap.Text) > previewMaxText {
		t.Errorf("text kept at %d chars, cap is %d", len(snap.Text), previewMaxText)
	}
	if len(snap.Elements) > previewMaxElements {
		t.Errorf("elements kept at %d, cap is %d", len(snap.Elements), previewMaxElements)
	}
	if len(snap.Errors) > previewMaxErrors {
		t.Errorf("errors kept at %d, cap is %d", len(snap.Errors), previewMaxErrors)
	}
}

func TestPreviewAgentShimIsScopedAndDefensive(t *testing.T) {
	out := injectThemeShim([]byte("<html><head></head><body></body></html>"))

	for _, want := range []string{
		"preview/runtime",    // it reports to the daemon
		"sessions",           // scoped by the session in its own URL
		"digi:preview-error", // the existing shims are untouched
		"digi:nav",           // idem
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("injected HTML missing %q", want)
		}
	}
	// A page that is not a session-scoped preview must never report: the shim
	// bails out when its own URL does not match, which is what keeps the shared
	// /web-static app bundles out of any session's state.
	if !bytes.Contains(out, []byte("if(!m)return")) {
		t.Error("the agent shim must self-disable outside a session preview")
	}
	if !bytes.Contains(out, []byte("if(!tok)return")) {
		t.Error("the agent shim must self-disable without a preview token")
	}
	// The user watches the agent drive their app, so the driving must be
	// legible: a badge while it acts, a ring on what it touches. That layer
	// must never be mistaken for the app itself, so it sits outside <body>
	// (kept out of the text the agent reads back) and cannot be clicked.
	// Debugging the app the way a human would: a failed request and a plain
	// console.log are invisible to the compiler and to the error shim, yet they
	// are the usual reason data never appears on screen.
	for _, want := range []string{"failed_requests", "XMLHttpRequest.prototype.open", "cap(\"log\")"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("network/console capture missing %q", want)
		}
	}
	// Measured layout beats a picture an agent reads poorly: these are the
	// defects a screenshot would be used to spot, expressed as numbers it can
	// act on.
	for _, want := range []string{"overflow_x", "tiny_text", "low_contrast", "localStorage", "elementFromPoint"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("visual audit missing %q", want)
		}
	}
	// A click must look like a real one. ``el.click()`` alone fires only
	// "click", so a Radix/shadcn menu — which craft mandates and which opens on
	// pointerdown — never reacts, and the agent concludes its own component is
	// broken. Typing must emit real keystrokes for the same reason: masks and
	// live search boxes listen to keys, not to a value assignment.
	for _, want := range []string{"pointerdown", "mousedown", "mouseup", "beforeinput", "keydown"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("human interaction missing %q", want)
		}
	}
	// And it must FAIL where a user would fail. Clicking through a covering
	// overlay would report a success the user cannot reproduce, which is worse
	// than reporting nothing.
	for _, want := range []string{"reachable", "covers it", "is disabled", "elementFromPoint"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("reachability guard missing %q", want)
		}
	}
	// The gestures a human has and the agent needs to reach every corner.
	for _, want := range []string{"hover", "check", "select", "scroll", "viewport", "wait_for", "detail"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("gesture %q missing from the shim", want)
		}
	}
	for _, want := range []string{"data-digitorn", "pointer-events:none", "documentElement.appendChild"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("visual feedback layer missing %q", want)
		}
	}
	// Injected before </head> like its siblings, so it sees the first paint.
	if bytes.Index(out, []byte("preview/runtime")) > bytes.Index(out, []byte("</head>")) {
		t.Error("agent shim injected after </head>")
	}
}
