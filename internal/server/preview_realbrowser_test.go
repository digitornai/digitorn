package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"

	"github.com/digitornai/digitorn/internal/modules/preview"
)

// End-to-end against a REAL browser and a REAL built app.
//
// Everything else about the preview channel is verified in jsdom, which is fast
// and hermetic but is not a browser: it has no layout engine, no innerText, no
// real event dispatch and no CSS cascade. The guarantees that matter most here
// — that the page reports what a user actually sees, and that a queued command
// really drives React — can only be proven in Chrome.
//
// Point DIGITORN_PREVIEW_E2E_APP at a built Vite app (its dist directory) to
// run it:
//
//	npm create vite@latest app -- --template react-ts && cd app && npm i && npm run build
//	DIGITORN_PREVIEW_E2E_APP=$PWD/dist go test ./internal/server/ -run RealBrowser -v
func TestPreviewRealBrowser(t *testing.T) {
	dist := os.Getenv("DIGITORN_PREVIEW_E2E_APP")
	if dist == "" {
		t.Skip("set DIGITORN_PREVIEW_E2E_APP to a built app's dist directory")
	}
	if _, err := os.Stat(filepath.Join(dist, "index.html")); err != nil {
		t.Skipf("no index.html in %s", dist)
	}
	bin, ok := launcher.LookPath()
	if !ok {
		t.Skip("no Chrome on PATH")
	}

	const app, session = "craft", "e2e"
	d := previewTestDaemon()
	token := d.previewToken(app, session)
	defer preview.Shared().Forget(app, session)

	// The real handlers, mounted on the real paths: the page must find its own
	// app and session in its URL, or the shim disables itself.
	r := chi.NewRouter()
	r.Get("/api/apps/{app_id}/sessions/{session_id}/preview/serve/*", func(w http.ResponseWriter, req *http.Request) {
		rel := chi.URLParam(req, "*")
		if rel == "" {
			rel = "index.html"
		}
		abs := filepath.Join(dist, filepath.Clean("/"+rel))
		if strings.HasSuffix(abs, ".html") {
			serveHTMLWithShim(w, abs)
			return
		}
		http.ServeFile(w, req, abs)
	})
	r.Post("/api/apps/{app_id}/sessions/{session_id}/preview/runtime", d.postPreviewRuntime)

	srv := httptest.NewServer(r)
	defer srv.Close()

	ctrl, err := launcher.New().Headless(true).Bin(bin).
		Set("no-sandbox").Set("disable-gpu").Set("disable-dev-shm-usage").Launch()
	if err != nil {
		t.Skipf("could not launch Chrome: %v", err)
	}
	browser := rod.New().ControlURL(ctrl)
	if err := browser.Connect(); err != nil {
		t.Skipf("could not connect to Chrome: %v", err)
	}
	defer browser.Close()

	url := srv.URL + "/api/apps/" + app + "/sessions/" + session + "/preview/serve/index.html?t=" + token
	page, err := browser.Page(proto.TargetCreateTarget{URL: url})
	if err != nil {
		t.Fatalf("open page: %v", err)
	}
	defer page.Close()

	// ── The page reports itself ────────────────────────────────────────────
	var snap preview.Snapshot
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if s, seen := preview.Shared().Observe(app, session); seen && s.Ready {
			snap = s
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if snap.URL == "" {
		t.Fatal("the real app never reported to the daemon — the shim did not run, or could not reach the runtime endpoint")
	}

	t.Logf("url=%s title=%q elements=%d text=%dch viewport=%s",
		snap.URL, snap.Title, len(snap.Elements), len(snap.Text), snap.Viewport)

	if snap.Blank {
		t.Errorf("a real built app reported itself blank; errors=%+v", snap.Errors)
	}
	if len(snap.Text) == 0 {
		t.Error("no visible text: innerText did not work in a real browser, which jsdom could never have caught")
	}
	if len(snap.Elements) == 0 {
		t.Error("no actionable elements found in a real shadcn app")
	}
	if snap.Viewport == "" {
		t.Error("viewport not reported")
	}
	if snap.Layout == nil {
		t.Error("no layout measurement — the visual audit did not run against real CSS")
	} else {
		t.Logf("layout: overflow_x=%d tiny_text=%d low_contrast=%d",
			snap.Layout.OverflowX, snap.Layout.TinyText, snap.Layout.LowContrast)
		for _, sm := range snap.Layout.Samples {
			t.Logf("  sample: %s", sm)
		}
		// A well-built shadcn dashboard is not riddled with unreadable text.
		// An audit that cries wolf is worse than no audit: the agent would
		// spend its turn "fixing" defects that are not there.
		if snap.Layout.LowContrast > len(snap.Elements)/2 {
			t.Errorf("contrast flagged on %d elements out of %d — that is a false-positive rate, not a finding",
				snap.Layout.LowContrast, len(snap.Elements))
		}
	}

	// The overlay must never leak into what the agent reads back.
	if strings.Contains(snap.Text, "Digitorn") {
		t.Error("the feedback overlay leaked into the page text the agent reads")
	}

	// ── A queued command really drives the app ─────────────────────────────
	var target string
	for _, e := range snap.Elements {
		if e.Role == "button" && strings.TrimSpace(e.Text) != "" {
			target = e.Text
			break
		}
	}
	if target == "" {
		t.Skip("no labelled button in this app to click")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	after, err := preview.Shared().Submit(ctx, app, session,
		preview.Command{ID: "e2e-1", Do: "click", TextMatch: target})
	if err != nil {
		t.Fatalf("clicking %q in a real browser failed: %v", target, err)
	}
	t.Logf("clicked %q, page answered in %v", target, time.Since(start))

	if after.URL == "" {
		t.Error("no state came back after the click")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the click round trip took %v; held requests are supposed to make this immediate", time.Since(start))
	}

	// ── Observing costs one round trip and returns fresh state ─────────────
	start = time.Now()
	if _, err := preview.Shared().Submit(ctx, app, session,
		preview.Command{ID: "e2e-2", Do: "observe"}); err != nil {
		t.Fatalf("observe failed: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("a plain observe took %v", d)
	}
}
