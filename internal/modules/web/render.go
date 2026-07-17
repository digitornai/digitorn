package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/modules/eventemitter"
	"github.com/digitornai/digitorn/internal/runtime/workdir"
	"github.com/digitornai/digitorn/internal/safehttp"
)

// browserEngine owns ONE headless Chromium (launched lazily on first use) and a
// pool of persistent tabs keyed by session: a session drives the SAME live tab
// across fetch/action calls, so cookies, localStorage, scroll and JS state
// persist — that is what turns stateless fetches into real navigation. Every
// request any tab makes is vetted by the same SSRF guard as the HTTP path
// (safehttp.BlockedIP), via CDP request interception, so a rendered page can
// never reach loopback/private/link-local/cloud-metadata addresses.
type browserEngine struct {
	mu        sync.Mutex
	instances map[string]*browserInstance
	stop      chan struct{}
}

// browserInstance is one Chrome process. Ephemeral instances are headless with a
// throwaway profile (anonymous fetch/search/crawl); a profile instance is a
// VISIBLE window backed by a persistent on-disk user-data-dir, so logins the
// user establishes survive daemon restarts. One instance per identity keeps one
// user's cookies out of another's.
type browserInstance struct {
	browser *rod.Browser
	profile bool
	tabs    map[string]*sessionTab
}

// Idle policy: a tab unused for its idle window is closed; when an instance has
// no tab left, the Chrome process is shut down (profile data stays on disk).
// Profile tabs get a longer window: the visible browser may be sitting on a
// login or CAPTCHA the user is solving by hand, and must not vanish mid-action.
const (
	tabIdle        = 3 * time.Minute
	profileTabIdle = 20 * time.Minute
	janitorPeriod  = 1 * time.Minute
)

// actionElementWait bounds ref→element resolution; actionTimeout bounds the
// whole interaction (scroll/click/settle) so no single action hangs on the
// page's navigation timeout.
const (
	actionElementWait = 4 * time.Second
	actionTimeout     = 8 * time.Second
)

// sessionTab is one persistent browser tab bound to a session, with its SSRF
// hijack router running for the tab's whole life.
type sessionTab struct {
	page     *rod.Page
	router   *rod.HijackRouter
	lastUsed time.Time
	profile  bool // backed by a persistent visible profile → longer idle window

	// liveMu guards the live fields AND url (read by the live goroutine).
	liveMu       sync.Mutex
	url          string
	liveOn       bool
	liveCancel   context.CancelFunc
	liveEmitCtx  context.Context
	liveDeadline time.Time
}

// liveLinger keeps the screencast alive this long after the last live call, so
// the view survives the agent's thinking gaps. Cheap: a static page emits no
// frames.
const liveLinger = 30 * time.Second

// refAttr is the DOM attribute the browser stamps on every element in document
// order; the page model reads it so a ref resolves to the exact live element an
// action targets. Stable per DOM snapshot, re-stamped on every perception.
const refAttr = "data-dgn-ref"

// hiddenAttr marks elements with no layout box (closed dropdowns, collapsed
// menus). They stay in the model but rank last, so a mega-list of hidden
// options can't crowd out what is actually on screen.
const hiddenAttr = "data-dgn-hidden"

// checkedAttr mirrors the live .checked PROPERTY (which clicking sets) into an
// attribute the HTML-parsing perception can read — the HTML `checked` attribute
// alone never reflects a runtime selection.
const checkedAttr = "data-dgn-checked"

// annotateJS stamps a ref on every element that lacks one, marks the ones with
// no layout box as hidden, and returns the annotated outerHTML. Only NEW
// elements get a number, so a node that survives a re-render keeps its ref, and
// a ref whose node was replaced resolves to nothing (a loud "not found") instead
// of silently pointing at a different element. Visibility is read in one pass
// before any write so we force a single layout, not one per element.
const annotateJS = `() => {
  if (!window.__dgnSeq) window.__dgnSeq = 0;
  const els = Array.from(document.querySelectorAll('*'));
  const shown = els.map(el => el.getClientRects().length > 0);
  for (let i = 0; i < els.length; i++) {
    const el = els[i];
    if (!el.hasAttribute('` + refAttr + `')) {
      el.setAttribute('` + refAttr + `', 'e' + (++window.__dgnSeq));
    }
    if (shown[i]) el.removeAttribute('` + hiddenAttr + `');
    else el.setAttribute('` + hiddenAttr + `', '1');
    if (el.tagName === 'INPUT' && (el.type === 'radio' || el.type === 'checkbox')) {
      if (el.checked) el.setAttribute('` + checkedAttr + `', '1');
      else el.removeAttribute('` + checkedAttr + `');
    }
  }
  return document.documentElement.outerHTML;
}`

func newBrowserEngine() *browserEngine {
	e := &browserEngine{instances: map[string]*browserInstance{}, stop: make(chan struct{})}
	go e.janitor()
	return e
}

// janitor periodically reaps idle tabs and shuts the browser when idle.
func (e *browserEngine) janitor() {
	t := time.NewTicker(janitorPeriod)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
			e.reap(tabIdle)
		}
	}
}

// instanceKeyFor derives the browser-instance key from the caller's identity.
// Ephemeral work is isolated per user; profile work is isolated per user+app so
// one app's logins never leak to another.
func instanceKeyFor(ctx context.Context, profile bool) (key, user, app string) {
	id, _ := tool.IdentityFromContext(ctx)
	user = firstNonEmpty(id.UserID, "default")
	app = firstNonEmpty(id.AppID, "default")
	if profile {
		return "profile:" + user + "/" + app, user, app
	}
	return "anon:" + user, user, app
}

// ensureInstance lazily launches the Chrome process for an instance key. Caller
// holds e.mu. Ephemeral = headless throwaway; profile = visible window on a
// persistent per-user+app user-data-dir (logins survive restarts).
func (e *browserEngine) ensureInstance(ctx context.Context, profile bool) (*browserInstance, error) {
	key, user, app := instanceKeyFor(ctx, profile)
	if inst, ok := e.instances[key]; ok && inst.browser != nil {
		return inst, nil
	}
	var l *launcher.Launcher
	if profile {
		dir, err := profileDir(user, app)
		if err != nil {
			return nil, err
		}
		l = launcherWithProfile(dir)
	} else {
		l = launcherWithDefaults()
	}
	ctrl, err := l.Launch()
	if err != nil {
		if profile {
			return nil, fmt.Errorf("visible browser profile unavailable (needs a desktop display): %w", err)
		}
		return nil, fmt.Errorf("headless browser unavailable: %w", err)
	}
	br := rod.New().ControlURL(ctrl)
	if err := br.Connect(); err != nil {
		return nil, fmt.Errorf("connect browser: %w", err)
	}
	inst := &browserInstance{browser: br, profile: profile, tabs: map[string]*sessionTab{}}
	e.instances[key] = inst
	return inst, nil
}

// acquireTab returns the session's live tab in the right instance, creating it
// (with the SSRF hijack router mounted) on first use. allowPrivate/domainOK
// define the egress policy enforced on every request the tab issues.
func (e *browserEngine) acquireTab(ctx context.Context, sessionKey string, profile, allowPrivate bool, domainOK func(string) error) (*sessionTab, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	inst, err := e.ensureInstance(ctx, profile)
	if err != nil {
		return nil, err
	}
	if t, ok := inst.tabs[sessionKey]; ok && t.page != nil {
		t.lastUsed = time.Now()
		return t, nil
	}
	page, err := inst.browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, fmt.Errorf("open tab: %w", err)
	}
	router := page.HijackRequests()
	router.MustAdd("*", func(h *rod.Hijack) {
		host := h.Request.URL().Hostname()
		if err := requestAllowed(host, allowPrivate, domainOK); err != nil {
			h.Response.Fail(proto.NetworkErrorReasonAccessDenied)
			return
		}
		h.ContinueRequest(&proto.FetchContinueRequest{})
	})
	go router.Run()

	t := &sessionTab{page: page, router: router, lastUsed: time.Now(), profile: profile}
	inst.tabs[sessionKey] = t
	return t, nil
}

// hasTab reports whether a live browser tab is already open for this session.
func (e *browserEngine) hasTab(ctx context.Context, sessionKey string, profile bool) bool {
	_, ok := e.lookupTab(ctx, sessionKey, profile)
	return ok
}

// lookupTab finds an already-open tab for the caller's identity (act path).
func (e *browserEngine) lookupTab(ctx context.Context, sessionKey string, profile bool) (*sessionTab, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key, _, _ := instanceKeyFor(ctx, profile)
	inst, ok := e.instances[key]
	if !ok {
		return nil, false
	}
	t, ok := inst.tabs[sessionKey]
	return t, ok && t.page != nil
}

// profileDir returns (creating it) the persistent user-data-dir for a user+app.
func profileDir(user, app string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot locate home dir for browser profile: %w", err)
	}
	dir := filepath.Join(home, ".digitorn", "browser", safeSeg(user), safeSeg(app))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create browser profile dir: %w", err)
	}
	return dir, nil
}

// safeSeg keeps a path segment to a safe alphabet so an identity can't escape
// the browser-profile root.
func safeSeg(s string) string {
	if s == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

// renderResult is what a perception (navigate/act + read) yields.
type renderResult struct {
	html     string
	finalURL string
	title    string
	shot     string // optional base64 JPEG data URL
}

// navigate points the session's tab at rawURL, waits for the DOM to settle,
// then reads the annotated HTML.
func (e *browserEngine) navigate(ctx context.Context, sessionKey, rawURL string, allowPrivate bool, timeout time.Duration, domainOK func(string) error, shot, live, profile bool) (renderResult, error) {
	t, err := e.acquireTab(ctx, sessionKey, profile, allowPrivate, domainOK)
	if err != nil {
		return renderResult{}, err
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	// Already on this exact URL → re-perceive instead of reloading. A reload
	// would wipe any form the agent just filled (agents re-pass the url to
	// "screenshot the page", which used to blow away all their input).
	reload := true
	if info, ierr := t.page.Info(); ierr == nil && info != nil && sameURL(info.URL, rawURL) {
		reload = false
	}
	t.setURL(rawURL) // up front so live frames during load carry the right url
	if live {
		t.ensureLive(ctx)
	}
	if reload {
		tp := t.page.Timeout(timeout)
		if err := tp.Navigate(rawURL); err != nil {
			return renderResult{}, fmt.Errorf("navigate: %w", err)
		}
		if err := tp.WaitLoad(); err != nil {
			return renderResult{}, fmt.Errorf("wait load: %w", err)
		}
		t.dismissConsent()
	}
	_ = t.page.Timeout(actionTimeout).WaitStable(700 * time.Millisecond)
	t.lastUsed = time.Now()
	return e.read(t, timeout, shot)
}

// sameURL reports whether two URLs point at the same page ignoring the fragment
// (so a "#section" jump isn't treated as a new page needing a reload).
func sameURL(a, b string) bool {
	na, ea := normalizeURL(a)
	nb, eb := normalizeURL(b)
	if ea != nil || eb != nil {
		return false
	}
	na.Fragment, nb.Fragment = "", ""
	return na.String() == nb.String()
}

// dismissConsent best-effort closes a cookie/consent overlay so the agent (and
// the live view) see the page instead of the banner. Silent when there is none.
func (t *sessionTab) dismissConsent() {
	_, _ = t.page.Timeout(3 * time.Second).Eval(dismissConsentJS)
}

const dismissConsentJS = `() => {
  const RX = /^(accept|accept all|allow all|i agree|agree|got it|ok|tout accepter|j'accepte|accepter|zustimmen|alle akzeptieren|aceptar|accetta)$/i;
  const pick = [];
  for (const sel of ['#onetrust-accept-btn-handler','.fc-cta-consent','[aria-label*="accept" i]','[id*="accept" i]','[class*="accept" i]']) {
    document.querySelectorAll(sel).forEach(e => pick.push(e));
  }
  for (const e of document.querySelectorAll('button,[role="button"],a')) {
    const txt = (e.innerText || e.textContent || '').trim();
    if (txt.length < 24 && RX.test(txt)) pick.push(e);
  }
  for (const e of pick) {
    const r = e.getBoundingClientRect();
    if (r.width > 0 && r.height > 0 && typeof e.click === 'function') { e.click(); return true; }
  }
  return false;
}`

// act applies a sequence of interactions to the session's live tab, then
// re-perceives. The tab must already be navigated (an agent perceives, then
// acts on what it saw).
func (e *browserEngine) act(ctx context.Context, sessionKey string, actions []actionSpec, allowPrivate bool, timeout time.Duration, domainOK func(string) error, shot, live, approvedSubmit, profile bool) (renderResult, error) {
	t, ok := e.lookupTab(ctx, sessionKey, profile)
	if !ok {
		return renderResult{}, fmt.Errorf("no active page for this session — fetch a url first, then act on it")
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if live {
		t.ensureLive(ctx)
	}
	for i, a := range actions {
		if err := e.applyAction(ctx, t, a, timeout, approvedSubmit); err != nil {
			return renderResult{}, fmt.Errorf("action %d (%s): %w", i+1, a.Do, err)
		}
	}
	_ = t.page.Timeout(actionTimeout).WaitStable(500 * time.Millisecond)
	if info, err := t.page.Info(); err == nil && info.URL != "" {
		t.setURL(info.URL) // a click may have navigated
	}
	t.lastUsed = time.Now()
	return e.read(t, timeout, shot)
}

// applyAction executes one interaction on the tab, resolving refs to the exact
// live element the last perception stamped.
func (e *browserEngine) applyAction(ctx context.Context, t *sessionTab, a actionSpec, timeout time.Duration, approvedSubmit bool) error {
	tp := t.page.Timeout(actionTimeout)
	switch strings.ToLower(strings.TrimSpace(a.Do)) {
	case "click":
		el, err := e.elementByRef(tp, a.Ref)
		if err != nil {
			return err
		}
		if !approvedSubmit {
			if err := guardSubmit(el.Timeout(actionTimeout), clickSubmitProbeJS); err != nil {
				return err
			}
		}
		return clickElement(el)
	case "type", "fill":
		el, err := e.elementByRef(tp, a.Ref)
		if err != nil {
			return err
		}
		if err := el.Focus(); err != nil {
			return err
		}
		return el.Input(a.Text)
	case "press":
		if !approvedSubmit && keyName(a.Key) == "enter" {
			if obj, err := tp.Eval(enterSubmitProbeJS); err == nil && obj.Value.Str() == "post" {
				return errSubmitNeedsApproval
			}
		}
		key := keyByName(a.Key)
		return tp.Keyboard.Type(key)
	case "select":
		el, err := e.elementByRef(tp, a.Ref)
		if err != nil {
			return err
		}
		if strings.TrimSpace(a.Text) == "" {
			return fmt.Errorf("select needs text: the visible label of the option to pick")
		}
		return el.Timeout(actionTimeout).Select([]string{a.Text}, true, rod.SelectorTypeText)
	case "check":
		el, err := e.elementByRef(tp, a.Ref)
		if err != nil {
			return err
		}
		obj, err := el.Timeout(actionTimeout).Eval(checkJS)
		if err != nil {
			return err
		}
		if !obj.Value.Bool() {
			return fmt.Errorf("check: no radio/checkbox found at or near ref %q — pass the ref of the option (or its label)", a.Ref)
		}
		return nil
	case "hover":
		el, err := e.elementByRef(tp, a.Ref)
		if err != nil {
			return err
		}
		return el.Timeout(actionTimeout).Hover()
	case "upload":
		return e.uploadFile(ctx, tp, a)
	case "scroll":
		js := "() => window.scrollBy(0, window.innerHeight)"
		switch strings.ToLower(a.To) {
		case "bottom":
			js = "() => window.scrollTo(0, document.body.scrollHeight)"
		case "top":
			js = "() => window.scrollTo(0, 0)"
		}
		_, err := tp.Eval(js)
		return err
	case "wait":
		return e.waitFor(t, t.page.Timeout(timeout), a, timeout)
	default:
		return fmt.Errorf("unknown action %q (use click|type|press|select|check|hover|upload|scroll|wait)", a.Do)
	}
}

// checkJS ticks the radio/checkbox at or near the ref. A ref often lands on the
// styled label/wrapper, not the real (often visually hidden) input, so it
// resolves the input via for=, a descendant, or the enclosing label, then calls
// input.click() — the one method that flips the control AND fires the click/
// input/change events React listens to. Returns true once the input is checked.
const checkJS = `() => {
  let input = null;
  const isBox = el => el && el.tagName === 'INPUT' && (el.type === 'radio' || el.type === 'checkbox');
  if (isBox(this)) input = this;
  else if (this.tagName === 'LABEL' && this.htmlFor) { const t = document.getElementById(this.htmlFor); if (isBox(t)) input = t; }
  if (!input && this.querySelector) input = this.querySelector('input[type=radio],input[type=checkbox]');
  if (!input) {
    const lab = this.closest && this.closest('label');
    if (lab) input = lab.querySelector('input[type=radio],input[type=checkbox]') || (lab.htmlFor ? document.getElementById(lab.htmlFor) : null);
  }
  if (!isBox(input)) return false;
  if (!input.checked) input.click();
  input.dispatchEvent(new Event('input', { bubbles: true }));
  input.dispatchEvent(new Event('change', { bubbles: true }));
  return input.checked === true;
}`

// uploadFile sets a file on an <input type=file>. The path is confined to the
// session's workspace via the same PathPolicy every file tool enforces — the
// browser can never be used to exfiltrate an arbitrary daemon file.
func (e *browserEngine) uploadFile(ctx context.Context, tp *rod.Page, a actionSpec) error {
	if strings.TrimSpace(a.Path) == "" {
		return fmt.Errorf("upload needs path: a file inside the session workspace")
	}
	pp, ok := workdir.PathPolicyFromContext(ctx)
	if !ok || !pp.HasWorkdir() {
		return fmt.Errorf("upload unavailable: no session workspace to read files from")
	}
	resolved, err := pp.Enforce(a.Path)
	if err != nil {
		return fmt.Errorf("upload path: %w", err)
	}
	if _, err := os.Stat(resolved); err != nil {
		return fmt.Errorf("upload file not found in workspace: %s", a.Path)
	}
	el, err := e.elementByRef(tp, a.Ref)
	if err != nil {
		return err
	}
	// A styled uploader's ref often lands on the visible button; the real
	// <input type=file> is a hidden sibling. Retarget when needed.
	if isFile, _ := el.Eval(`() => this.tagName === 'INPUT' && this.type === 'file'`); isFile == nil || !isFile.Value.Bool() {
		if alt, aerr := tp.Timeout(actionTimeout).Element(`input[type="file"]`); aerr == nil {
			el = alt
		} else {
			return fmt.Errorf("ref %q is not a file input and none was found on the page", a.Ref)
		}
	}
	return el.Timeout(actionTimeout).SetFiles([]string{resolved})
}

// errSubmitNeedsApproval blocks consequential form submissions (method=POST)
// until the agent confirms with the user and retries with approved_submit.
// GET forms (search boxes, filters) pass freely.
var errSubmitNeedsApproval = fmt.Errorf("this action submits a form (method=POST) — a consequential submission. Show the user exactly what will be submitted, get their explicit confirmation, then retry with approved_submit:true")

func guardSubmit(el *rod.Element, probeJS string) error {
	obj, err := el.Eval(probeJS)
	if err == nil && obj.Value.Str() == "post" {
		return errSubmitNeedsApproval
	}
	return nil
}

// clickSubmitProbeJS: does clicking this element submit a form, and with what
// method? Empty string = not a submission.
const clickSubmitProbeJS = `() => {
  const t = (this.closest && this.closest('a,button,[role="button"],input,summary,label')) || this;
  const f = t.form || (t.closest && t.closest('form'));
  if (!f) return '';
  const tag = t.tagName, typ = (t.type || '').toLowerCase();
  const submits = (tag === 'BUTTON' && (typ === '' || typ === 'submit')) || (tag === 'INPUT' && (typ === 'submit' || typ === 'image'));
  return submits ? (f.method || 'get').toLowerCase() : '';
}`

// enterSubmitProbeJS: would pressing Enter now submit a form? (focus sits in a
// form field). Returns the form's method, or empty.
const enterSubmitProbeJS = `() => {
  const el = document.activeElement;
  if (!el || el.tagName === 'TEXTAREA') return '';
  const f = el.form || (el.closest && el.closest('form'));
  return f ? (f.method || 'get').toLowerCase() : '';
}`

func keyName(k string) string { return strings.ToLower(strings.TrimSpace(k)) }

func (e *browserEngine) waitFor(t *sessionTab, tp *rod.Page, a actionSpec, timeout time.Duration) error {
	switch {
	case a.MS > 0:
		d := time.Duration(a.MS) * time.Millisecond
		if d > timeout {
			d = timeout
		}
		time.Sleep(d)
		return nil
	case a.For == "networkidle" || a.For == "":
		return t.page.WaitStable(1 * time.Second)
	default: // treat For as a CSS selector to wait for
		_, err := tp.Element(a.For)
		return err
	}
}

// clickElement scrolls the element into view and clicks it, falling back to a
// DOM click when a real mouse click can't land (covered, off-screen, or
// pointer-events:none) so navigation isn't blocked by overlays.
func clickElement(el *rod.Element) error {
	if err := el.Timeout(2*time.Second).Click(proto.InputMouseButtonLeft, 1); err == nil {
		return nil
	}
	_, err := el.Timeout(2 * time.Second).Eval(clickFallbackJS)
	return err
}

// clickFallbackJS clicks the nearest interactive ancestor (a ref often lands on
// an icon/span inside the real button) and falls back to dispatching a mouse
// event, since SVG elements have no .click().
const clickFallbackJS = `() => {
  const t = (this.closest && this.closest('a,button,[role="button"],[role="tab"],input,summary,label,[onclick]')) || this;
  if (typeof t.click === 'function') { t.click(); return true; }
  t.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, view: window }));
  return true;
}`

// elementByRef resolves a ref stamped by the last perception to its live element.
func (e *browserEngine) elementByRef(tp *rod.Page, ref string) (*rod.Element, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("missing ref")
	}
	sel := "[" + refAttr + "='" + strings.ReplaceAll(ref, "'", "") + "']"
	has, _, err := tp.Timeout(actionElementWait).Has(sel)
	if err != nil || !has {
		return nil, fmt.Errorf("element %q not found on current page (it may have changed — re-fetch to get fresh refs)", ref)
	}
	return tp.Element(sel) // fresh context: the fast check bounded the lookup, not the action
}

// read stamps document-order refs, returns the annotated HTML + metadata, and
// optionally a screenshot.
func (e *browserEngine) read(t *sessionTab, timeout time.Duration, shot bool) (renderResult, error) {
	tp := t.page.Timeout(timeout)
	obj, err := tp.Eval(annotateJS)
	if err != nil {
		return renderResult{}, fmt.Errorf("read html: %w", err)
	}
	res := renderResult{html: obj.Value.Str(), finalURL: t.currentURL()}
	if info, ierr := t.page.Info(); ierr == nil && info != nil {
		res.finalURL = info.URL
		res.title = info.Title
	}
	if shot {
		if b, serr := t.page.Timeout(timeout).Screenshot(false, &proto.PageCaptureScreenshot{
			Format:  proto.PageCaptureScreenshotFormatJpeg,
			Quality: intPtr(55),
		}); serr == nil && len(b) > 0 {
			res.shot = "data:image/jpeg;base64," + base64Std(b)
		}
	}
	return res, nil
}

func (t *sessionTab) setURL(u string) {
	t.liveMu.Lock()
	t.url = u
	t.liveMu.Unlock()
}

func (t *sessionTab) currentURL() string {
	t.liveMu.Lock()
	defer t.liveMu.Unlock()
	return t.url
}

// ensureLive starts (or, if already running, extends) a persistent CDP
// screencast that streams this tab's frames to the session's client. The emit
// context is detached from the per-call ctx so frames outlive the tool call;
// liveWatch stops it liveLinger after the last live call.
func (t *sessionTab) ensureLive(callCtx context.Context) {
	t.liveMu.Lock()
	t.liveDeadline = time.Now().Add(liveLinger)
	if t.liveOn {
		t.liveMu.Unlock()
		return
	}
	if err := (proto.PageStartScreencast{
		Format:        proto.PageStartScreencastFormatJpeg,
		Quality:       intPtr(75),
		EveryNthFrame: intPtr(2),
		MaxWidth:      intPtr(1440),
		MaxHeight:     intPtr(900),
	}).Call(t.page); err != nil {
		t.liveMu.Unlock()
		return
	}
	emitCtx, cancel := context.WithCancel(context.WithoutCancel(callCtx))
	t.liveOn = true
	t.liveCancel = cancel
	t.liveEmitCtx = emitCtx
	p := t.page
	t.liveMu.Unlock()

	// Explicit start/stop beats inferring "streaming" from frame arrival: a
	// static page emits no frames while the agent thinks.
	eventemitter.EmitWithModule(emitCtx, "web", "web.browser.live", map[string]any{
		"active": true,
		"url":    t.currentURL(),
	})

	go p.Context(emitCtx).EachEvent(func(e *proto.PageScreencastFrame) {
		// base64: a []byte would ride socket.io as a binary attachment; the
		// client wants a string for a data: URL.
		eventemitter.EmitWithModule(emitCtx, "web", "web.browser.frame", map[string]any{
			"frame": base64Std(e.Data),
			"url":   t.currentURL(),
		})
		_ = (proto.PageScreencastFrameAck{SessionID: e.SessionID}).Call(p)
	})()
	go t.liveWatch()
}

func (t *sessionTab) liveWatch() {
	tk := time.NewTicker(2 * time.Second)
	defer tk.Stop()
	for range tk.C {
		t.liveMu.Lock()
		on, dl := t.liveOn, t.liveDeadline
		t.liveMu.Unlock()
		if !on {
			return
		}
		if time.Now().After(dl) {
			t.stopLive()
			return
		}
	}
}

// stopLive halts the screencast and its emit goroutine. Idempotent.
func (t *sessionTab) stopLive() {
	t.liveMu.Lock()
	if !t.liveOn {
		t.liveMu.Unlock()
		return
	}
	t.liveOn = false
	cancel := t.liveCancel
	emitCtx := t.liveEmitCtx
	t.liveCancel = nil
	t.liveEmitCtx = nil
	page := t.page
	t.liveMu.Unlock()

	if emitCtx != nil {
		eventemitter.EmitWithModule(emitCtx, "web", "web.browser.live", map[string]any{"active": false})
	}
	if cancel != nil {
		cancel()
	}
	if page != nil {
		_ = (proto.PageStopScreencast{}).Call(page)
	}
}

// closeTab tears down one session's tab (router + page).
func (e *browserEngine) closeTab(ctx context.Context, sessionKey string, profile bool) {
	e.mu.Lock()
	key, _, _ := instanceKeyFor(ctx, profile)
	var t *sessionTab
	if inst, ok := e.instances[key]; ok {
		t = inst.tabs[sessionKey]
		delete(inst.tabs, sessionKey)
	}
	e.mu.Unlock()
	closeTabResources(t)
}

// reap closes tabs idle past their window (profile tabs get longer), and shuts
// down any instance left with no tab. Profile Chrome exits but its user-data-dir
// stays on disk, so logins survive. Returns whether anything was shut down.
func (e *browserEngine) reap(idle time.Duration) bool {
	e.mu.Lock()
	now := time.Now()
	shutdown := false
	for key, inst := range e.instances {
		win := idle
		if inst.profile {
			win = profileTabIdle
		}
		for k, t := range inst.tabs {
			if now.Sub(t.lastUsed) > win {
				delete(inst.tabs, k)
				go closeTabResources(t)
			}
		}
		if len(inst.tabs) == 0 && inst.browser != nil {
			br := inst.browser
			delete(e.instances, key)
			shutdown = true
			go func() { _ = br.Close() }()
		}
	}
	e.mu.Unlock()
	return shutdown
}

// close tears everything down (module Stop): stops the janitor, all tabs, and
// every Chrome process.
func (e *browserEngine) close() {
	e.mu.Lock()
	if e.stop != nil {
		close(e.stop)
		e.stop = nil
	}
	instances := e.instances
	e.instances = map[string]*browserInstance{}
	e.mu.Unlock()
	for _, inst := range instances {
		for _, t := range inst.tabs {
			closeTabResources(t)
		}
		if inst.browser != nil {
			_ = inst.browser.Close()
		}
	}
}

func closeTabResources(t *sessionTab) {
	if t == nil {
		return
	}
	t.stopLive()
	if t.router != nil {
		_ = t.router.Stop()
	}
	if t.page != nil {
		_ = t.page.Close()
	}
}

// requestAllowed enforces the domain policy then the SSRF IP guard for a host a
// browser request is about to reach. allowPrivate drops the IP guard (operator
// opt-in, same semantics as the HTTP client).
func requestAllowed(host string, allowPrivate bool, domainOK func(string) error) error {
	if host == "" {
		return fmt.Errorf("empty host")
	}
	if domainOK != nil {
		if err := domainOK(host); err != nil {
			return err
		}
	}
	if allowPrivate {
		return nil
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		if safehttp.BlockedIP(ip) {
			return fmt.Errorf("blocked address %s", ip)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ipa := range ips {
		if safehttp.BlockedIP(ipa.IP) {
			return fmt.Errorf("%s resolves to forbidden address %s", host, ipa.IP)
		}
	}
	return nil
}

// looksLikeJSShell reports whether html is a client-render shell whose real
// content only appears after JS runs — the signal to fall back to a headless
// render.
func looksLikeJSShell(html, visibleText string) bool {
	vt := strings.TrimSpace(visibleText)
	if len(vt) > 400 {
		return false
	}
	lower := strings.ToLower(html)
	mounts := []string{
		`id="root"`, `id="app"`, `id="__next"`, `id="__nuxt"`,
		`data-reactroot`, `ng-version`, `<div id=root`,
	}
	for _, m := range mounts {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return strings.Count(lower, "<script") >= 3 && len(vt) < 200
}

// launcherWithDefaults builds a headless-launch config, preferring a system
// Chrome/Chromium on PATH (no download).
func launcherWithDefaults() *launcher.Launcher {
	l := launcher.New().Headless(true).
		Set("no-sandbox").
		Set("disable-gpu").
		Set("disable-dev-shm-usage").
		Set("disable-background-networking").
		Set("mute-audio")
	if bin, ok := launcher.LookPath(); ok {
		l = l.Bin(bin)
	}
	return l
}

// launcherWithProfile builds a VISIBLE (headful) launch on a persistent
// user-data-dir, so the user can log in / solve a CAPTCHA in the window and
// their sessions persist. Chrome refuses --no-sandbox+headful as root, but the
// daemon runs as the user, so the flags mirror the desktop case.
func launcherWithProfile(dir string) *launcher.Launcher {
	l := launcher.New().Headless(false).
		Set("no-sandbox").
		Set("disable-dev-shm-usage").
		Set("disable-background-networking").
		Set("mute-audio").
		Set("no-first-run").
		Set("no-default-browser-check").
		Set("user-data-dir", dir)
	if bin, ok := launcher.LookPath(); ok {
		l = l.Bin(bin)
	}
	return l
}

func intPtr(v int) *int { return &v }

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func keyByName(name string) input.Key {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "enter", "return", "":
		return input.Enter
	case "tab":
		return input.Tab
	case "escape", "esc":
		return input.Escape
	case "backspace":
		return input.Backspace
	case "arrowdown", "down":
		return input.ArrowDown
	case "arrowup", "up":
		return input.ArrowUp
	default:
		return input.Enter
	}
}
