package preview

import (
	"context"
	"sync"
	"time"
)

// Store holds what the previewed app reports about itself, and the commands the
// agent wants it to run, strictly partitioned per session.
//
// Isolation is the whole point. Every entry is filed under a key built from the
// app id AND the session id, and callers never pass that key themselves: the
// HTTP side derives it from the preview token (an HMAC over app+session, so a
// forged pair cannot validate) and the tool side derives it from the request
// identity the runtime attaches. There is no lookup by session id alone and no
// iteration across sessions, so session A has no reachable path to session B's
// state — not a policy check that could be forgotten, but the absence of an API
// to do it.
//
// Everything is in-memory and bounded: a preview is live state, worthless once
// the process restarts, and must never grow without limit because the page that
// feeds it is the agent's own possibly-broken output.
type Store struct {
	mu       sync.Mutex
	sessions map[key]*state
}

type key struct{ app, session string }

const (
	maxErrors      = 50
	maxQueued      = 8
	staleAfter     = 90 * time.Second
	commandTimeout = 15 * time.Second
)

// RuntimeError is a failure the page reported about itself.
type RuntimeError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Source  string `json:"source,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Stack   string `json:"stack,omitempty"`
	Count   int    `json:"count,omitempty"`
}

// Element is one thing on screen the agent can read or act on. Ref is the handle
// the page assigned it; it is only valid for the snapshot it came from, because
// a re-render replaces the nodes it points at.
type Element struct {
	Ref   string `json:"ref"`
	Role  string `json:"role"`
	Text  string `json:"text,omitempty"`
	Level int    `json:"level,omitempty"`
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
	Href  string `json:"href,omitempty"`
}

// Request is a network call the page made that did not succeed. A vibecoder's
// app most often fails here rather than in the code: the endpoint 404s, the key
// is missing, CORS blocks it. Nothing in the console says so when the call is
// awaited inside a try/catch, so the page reports them explicitly.
type Request struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Status int    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// LogLine is a console message that is not an error — what the agent printed on
// purpose to understand its own code.
type LogLine struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// Layout is what the page looks like MEASURED rather than pictured. An agent
// reads a picture poorly but acts precisely on "content overflows by 47px" —
// and these are exactly the defects a screenshot would be used to spot.
type Layout struct {
	// OverflowX is how many pixels of content sit beyond the viewport width.
	// Non-zero means the user scrolls sideways, the most common way a layout
	// breaks on a phone.
	OverflowX int `json:"overflow_x,omitempty"`
	// TinyText counts visible text rendered below 12px, unreadable on mobile.
	TinyText int `json:"tiny_text,omitempty"`
	// LowContrast counts text whose contrast against its background falls under
	// the readable threshold.
	LowContrast int `json:"low_contrast,omitempty"`
	// Samples name a few offending elements so the fix has somewhere to land.
	Samples []string `json:"samples,omitempty"`
}

// Detail is everything about one element, for when it is on screen but does not
// behave: disabled, covered by something else, zero-sized, wired to nothing.
type Detail struct {
	Ref      string            `json:"ref"`
	Tag      string            `json:"tag,omitempty"`
	Attrs    map[string]string `json:"attrs,omitempty"`
	Styles   map[string]string `json:"styles,omitempty"`
	Rect     string            `json:"rect,omitempty"`
	Covered  bool              `json:"covered_by_another_element,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
	HTML     string            `json:"html,omitempty"`
}

// Snapshot is what the page looks like right now.
type Snapshot struct {
	URL      string            `json:"url"`
	Title    string            `json:"title,omitempty"`
	Ready    bool              `json:"ready"`
	Blank    bool              `json:"blank"`
	Text     string            `json:"text,omitempty"`
	Elements []Element         `json:"elements,omitempty"`
	Errors   []RuntimeError    `json:"errors,omitempty"`
	Failed   []Request         `json:"failed_requests,omitempty"`
	Logs     []LogLine         `json:"logs,omitempty"`
	Viewport string            `json:"viewport,omitempty"`
	Layout   *Layout           `json:"layout,omitempty"`
	Storage  map[string]string `json:"storage,omitempty"`
	Detail   *Detail           `json:"detail,omitempty"`
	At       time.Time         `json:"at"`
}

// Command is an instruction waiting for the page to pick up.
type Command struct {
	ID      string         `json:"id"`
	Do      string         `json:"do"`
	Ref     string         `json:"ref,omitempty"`
	Text    string         `json:"text,omitempty"`
	Key     string         `json:"key,omitempty"`
	URL     string         `json:"url,omitempty"`
	Timeout int            `json:"timeout,omitempty"`
	Extra   map[string]any `json:"extra,omitempty"`
}

type state struct {
	snap     Snapshot
	errors   []RuntimeError
	queue    []Command
	waiters  map[string]chan Snapshot
	lastSeen time.Time
}

func NewStore() *Store { return &Store{sessions: map[key]*state{}} }

func (s *Store) at(k key) *state {
	st, ok := s.sessions[k]
	if !ok {
		st = &state{waiters: map[string]chan Snapshot{}}
		s.sessions[k] = st
	}
	return st
}

// Report records the page's current state. Errors accumulate across reports
// (deduplicated) because the agent may only look after several have piled up,
// while the rest of the snapshot is simply replaced by the latest truth.
func (s *Store) Report(app, session string, snap Snapshot) {
	if app == "" || session == "" {
		return
	}
	k := key{app, session}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.at(k)
	st.lastSeen = time.Now()
	snap.At = st.lastSeen
	incoming := snap.Errors
	snap.Errors = nil
	st.snap = snap
	for _, e := range incoming {
		st.appendError(e)
	}
}

func (st *state) appendError(e RuntimeError) {
	for i := range st.errors {
		if st.errors[i].Kind == e.Kind && st.errors[i].Message == e.Message && st.errors[i].Line == e.Line {
			st.errors[i].Count++
			return
		}
	}
	e.Count = 1
	st.errors = append(st.errors, e)
	if len(st.errors) > maxErrors {
		st.errors = st.errors[len(st.errors)-maxErrors:]
	}
}

// Observe returns the last known state of this session's preview. The bool
// reports whether the page has ever checked in: distinguishing "no preview
// running" from "a preview that is fine" is the difference between the agent
// waiting and the agent moving on.
func (s *Store) Observe(app, session string) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.sessions[key{app, session}]
	if !ok || st.lastSeen.IsZero() {
		return Snapshot{}, false
	}
	out := st.snap
	out.Errors = append([]RuntimeError(nil), st.errors...)
	return out, true
}

// Live reports whether the page checked in recently enough to still be driving.
func (s *Store) Live(app, session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.sessions[key{app, session}]
	return ok && !st.lastSeen.IsZero() && time.Since(st.lastSeen) < staleAfter
}

// ClearErrors drops the accumulated failures, so a rebuild starts from a clean
// slate instead of the agent re-reading crashes it has already fixed.
func (s *Store) ClearErrors(app, session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.sessions[key{app, session}]; ok {
		st.errors = nil
	}
}

// Submit queues a command and waits for the page to report back the state that
// resulted from it. It returns the post-action snapshot so acting and observing
// are one round trip rather than two.
func (s *Store) Submit(ctx context.Context, app, session string, cmd Command) (Snapshot, error) {
	k := key{app, session}
	ch := make(chan Snapshot, 1)

	s.mu.Lock()
	st := s.at(k)
	if len(st.queue) >= maxQueued {
		s.mu.Unlock()
		return Snapshot{}, ErrBusy
	}
	st.queue = append(st.queue, cmd)
	st.waiters[cmd.ID] = ch
	s.mu.Unlock()

	timer := time.NewTimer(commandTimeout)
	defer timer.Stop()
	select {
	case snap := <-ch:
		return snap, nil
	case <-ctx.Done():
		s.drop(k, cmd.ID)
		return Snapshot{}, ctx.Err()
	case <-timer.C:
		s.drop(k, cmd.ID)
		return Snapshot{}, ErrNoPreview
	}
}

func (s *Store) drop(k key, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.sessions[k]; ok {
		delete(st.waiters, id)
		for i, c := range st.queue {
			if c.ID == id {
				st.queue = append(st.queue[:i], st.queue[i+1:]...)
				break
			}
		}
	}
}

// Take hands the page its pending commands and marks the session as alive. The
// page polls this; an empty result is the normal case and must stay cheap.
func (s *Store) Take(app, session string) []Command {
	if app == "" || session == "" {
		return nil
	}
	k := key{app, session}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.at(k)
	st.lastSeen = time.Now()
	if len(st.queue) == 0 {
		return nil
	}
	out := st.queue
	st.queue = nil
	return out
}

// Complete delivers the state produced by a command back to the waiting caller.
// A result for an unknown id is dropped: the caller timed out or went away, and
// the page has no way to know that.
func (s *Store) Complete(app, session, id string, snap Snapshot) {
	k := key{app, session}
	s.mu.Lock()
	st, ok := s.sessions[k]
	if !ok {
		s.mu.Unlock()
		return
	}
	st.lastSeen = time.Now()
	snap.At = st.lastSeen
	incoming := snap.Errors
	snap.Errors = nil
	st.snap = snap
	for _, e := range incoming {
		st.appendError(e)
	}
	ch, waiting := st.waiters[id]
	if waiting {
		delete(st.waiters, id)
	}
	out := st.snap
	out.Errors = append([]RuntimeError(nil), st.errors...)
	s.mu.Unlock()

	if waiting {
		select {
		case ch <- out:
		default:
		}
	}
}

// Forget removes a session's preview state entirely.
func (s *Store) Forget(app, session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, key{app, session})
}
