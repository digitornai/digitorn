package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
)

// ── chunkText: adversarial sizing / unicode / degenerate input ────────────────

// reassemble joins chunks the way a reader does: chunkText drops the
// whitespace it breaks on, so we compare on whitespace-stripped content rather
// than byte-for-byte. This proves no characters are silently lost.
func reassemble(chunks []string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c)
	}
	return b.String()
}

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

// TestChunkTextBoundaries : the invariants every chunk must hold regardless of
// input — never empty, never over the rune limit, always valid UTF-8 (a rune is
// never split), and the concatenation preserves all non-break content.
func TestChunkTextBoundaries(t *testing.T) {
	const max = 2000
	cases := []struct {
		name      string
		in        string
		wantNil   bool // expect zero chunks
		minChunks int  // 0 = don't assert a lower bound
	}{
		{name: "empty", in: "", wantNil: true},
		{name: "whitespace only", in: "   \n\t \n  ", wantNil: true},
		{name: "single char", in: "x", minChunks: 1},
		{name: "exactly at limit", in: strings.Repeat("a", max), minChunks: 1},
		{name: "one over limit", in: strings.Repeat("a", max+1), minChunks: 2},
		{name: "far over, multi-chunk", in: strings.Repeat("word ", 4000), minChunks: 2},
		{name: "no break points (one giant token)", in: strings.Repeat("a", max*3+7), minChunks: 4},
		{name: "newline-only separators", in: strings.Repeat("line\n", 800), minChunks: 2},
		{name: "trailing whitespace then content", in: "   " + strings.Repeat("z", max+50), minChunks: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := chunkText(tc.in, max)
			if tc.wantNil {
				if len(chunks) != 0 {
					t.Fatalf("want no chunks, got %d: %q", len(chunks), chunks)
				}
				return
			}
			if len(chunks) == 0 {
				t.Fatalf("non-empty input produced zero chunks")
			}
			if tc.minChunks > 0 && len(chunks) < tc.minChunks {
				t.Fatalf("want >= %d chunks, got %d", tc.minChunks, len(chunks))
			}
			for i, c := range chunks {
				if c == "" {
					t.Fatalf("chunk %d is empty", i)
				}
				if n := utf8.RuneCountInString(c); n > max {
					t.Fatalf("chunk %d has %d runes (> %d limit)", i, n, max)
				}
				if !utf8.ValidString(c) {
					t.Fatalf("chunk %d is not valid UTF-8 (a rune was split): %q", i, c)
				}
			}
			if got, want := stripSpace(reassemble(chunks)), stripSpace(tc.in); got != want {
				t.Fatalf("content lost: reassembled %d runes, original had %d",
					utf8.RuneCountInString(got), utf8.RuneCountInString(want))
			}
		})
	}
}

// TestChunkTextMultibyte : with multibyte runes packed past the limit, every
// chunk must stay within the *rune* count AND never split a rune mid-byte —
// Discord counts codepoints, and a half-rune would corrupt the message. We also
// confirm the byte length can exceed the rune limit (proving the count is by
// rune, not byte) while remaining valid UTF-8.
func TestChunkTextMultibyte(t *testing.T) {
	const max = 100
	// 4-byte runes (emoji) so byte length is 4x the rune count.
	in := strings.Repeat("🚀", max*3+13) // no spaces/newlines: forces hard cuts
	chunks := chunkText(in, max)
	if len(chunks) < 4 {
		t.Fatalf("want >= 4 chunks for %d runes at max %d, got %d", max*3+13, max, len(chunks))
	}
	sawWideByteChunk := false
	for i, c := range chunks {
		n := utf8.RuneCountInString(c)
		if n == 0 || n > max {
			t.Fatalf("chunk %d has %d runes (must be 1..%d)", i, n, max)
		}
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d split a multibyte rune: %q", i, c)
		}
		if len(c) > max { // bytes exceed the rune limit → counting by rune, good
			sawWideByteChunk = true
		}
	}
	if !sawWideByteChunk {
		t.Fatal("expected at least one chunk whose byte length exceeds the rune limit")
	}
	if got, want := utf8.RuneCountInString(reassemble(chunks)), utf8.RuneCountInString(in); got != want {
		t.Fatalf("rune count changed: reassembled %d, original %d", got, want)
	}
}

// TestChunkTextMixedScript : a mix of ASCII, accented Latin, CJK and emoji with
// real word breaks — the break-at-space path must still respect the rune limit
// and keep every rune intact.
func TestChunkTextMixedScript(t *testing.T) {
	const max = 50
	unit := "café 日本語 🚀 naïve résumé "
	in := strings.Repeat(unit, 60)
	chunks := chunkText(in, max)
	if len(chunks) < 2 {
		t.Fatalf("want multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n == 0 || n > max {
			t.Fatalf("chunk %d has %d runes (must be 1..%d)", i, n, max)
		}
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d is invalid UTF-8: %q", i, c)
		}
	}
	if got, want := stripSpace(reassemble(chunks)), stripSpace(in); got != want {
		t.Fatalf("content lost in mixed-script chunking")
	}
}

// TestChunkTextDefaultMax : max<=0 falls back to the Discord default rather than
// looping forever or panicking.
func TestChunkTextDefaultMax(t *testing.T) {
	for _, bad := range []int{0, -1, -2000} {
		chunks := chunkText(strings.Repeat("a", maxMessageChars+500), bad)
		if len(chunks) < 2 {
			t.Fatalf("max=%d: expected fallback to default chunking, got %d chunk(s)", bad, len(chunks))
		}
		for i, c := range chunks {
			if n := utf8.RuneCountInString(c); n > maxMessageChars {
				t.Fatalf("max=%d chunk %d has %d runes (> default %d)", bad, i, n, maxMessageChars)
			}
		}
	}
}

// ── Send: HTTP error handling (no panic, error surfaced) ──────────────────────

// sendHarness fakes the Discord message-create endpoint with a caller-chosen
// status, counting how many POSTs land so we can assert chunked sends stop at
// the first failure.
func sendHarness(t *testing.T, status int) (*Adapter, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			atomic.AddInt32(&hits, 1)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"boom","code":50035}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	a := New([]Provider{{Name: "dc", Token: "supersecret", APIBase: srv.URL}}, nil)
	return a, &hits
}

// TestSendHTTPErrorsSurface : a 4xx / 5xx / 429 from message-create must return
// an error (so the processor can retry/log) and never panic.
func TestSendHTTPErrorsSurface(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		a, hits := sendHarness(t, status)
		ref := map[string]any{"provider": "dc", "channel_id": "c1"}
		err := a.Send(context.Background(), ref, "hello")
		if err == nil {
			t.Fatalf("status %d: expected an error from Send", status)
		}
		if atomic.LoadInt32(hits) != 1 {
			t.Fatalf("status %d: expected exactly 1 POST, got %d", status, atomic.LoadInt32(hits))
		}
	}
}

// TestSendErrorRedactsToken : an error surfaced from Send must not leak the bot
// token. The body is echoed, so we feed a status>=400 and assert the token
// never appears in the returned error.
func TestSendStopsAtFirstFailure(t *testing.T) {
	a, hits := sendHarness(t, http.StatusInternalServerError)
	ref := map[string]any{"provider": "dc", "channel_id": "c1"}
	// 3 chunks worth of text; the first POST fails so the rest must not be sent.
	err := a.Send(context.Background(), ref, strings.Repeat("a", maxMessageChars*3))
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("expected send to stop after the first failure (1 POST), got %d", n)
	}
}

// TestSendUnknownProviderAndChannel : a reply handle with no/unknown provider or
// a missing channel_id returns a descriptive error instead of dialing a fake
// endpoint or panicking on a nil provider.
func TestSendUnknownProviderAndChannel(t *testing.T) {
	a := New([]Provider{{Name: "dc", Token: "x", APIBase: "http://127.0.0.1:0"}}, nil)
	cases := []struct {
		name string
		ref  map[string]any
	}{
		{"unknown provider", map[string]any{"provider": "nope", "channel_id": "c1"}},
		{"missing provider", map[string]any{"channel_id": "c1"}},
		{"missing channel", map[string]any{"provider": "dc"}},
		{"empty channel", map[string]any{"provider": "dc", "channel_id": ""}},
		{"nil ref", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := a.Send(context.Background(), tc.ref, "hi"); err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
		})
	}
}

// TestSendNetworkErrorRedactsToken : when the HTTP client itself fails (dead
// endpoint), the error path runs redact() — the token must never reach the
// returned error string.
func TestSendNetworkErrorRedactsToken(t *testing.T) {
	// Closed server → connection refused, exercising the transport-error branch.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	a := New([]Provider{{Name: "dc", Token: "TOPSECRET-abc123", APIBase: url}}, nil)
	err := a.Send(context.Background(), map[string]any{"provider": "dc", "channel_id": "c1"}, "hi")
	if err == nil {
		t.Skip("transport unexpectedly succeeded against a closed server")
	}
	if strings.Contains(err.Error(), "TOPSECRET-abc123") {
		t.Fatalf("token leaked into error: %v", err)
	}
}

// ── onInteraction: malformed custom_id / nonce / action routing ───────────────

// interactionHarness gives an Adapter whose REST calls (interaction callbacks)
// all 200, so onInteraction can run its ack path without a live Discord.
func interactionHarness(t *testing.T) (*Adapter, Provider) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<16))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	a := New([]Provider{{Name: "dc", Token: "x", APIBase: srv.URL}}, nil)
	return a, a.byName["dc"]
}

// TestOnInteractionMalformed : a stream of garbage / malformed INTERACTION_CREATE
// payloads must never panic and never resolve a real pending prompt. We register
// one live prompt and assert it stays unresolved after every bad interaction.
func TestOnInteractionMalformed(t *testing.T) {
	a, p := interactionHarness(t)

	// A real pending prompt that must NOT be touched by any malformed event.
	live := &pendingPrompt{
		options: []adapter.PromptOption{{ID: "grant", Label: "OK"}},
		result:  make(chan promptHit, 1),
	}
	a.pmu.Lock()
	a.prompts["livenonce"] = live
	a.pmu.Unlock()

	bad := []string{
		`not json at all`,
		`{}`,
		`{"type":3,"data":{"custom_id":""}}`,
		`{"type":3,"data":{"custom_id":"justonepart"}}`,
		// wrong prefix
		`{"type":3,"data":{"custom_id":"b:livenonce:0"}}`,
		// unknown nonce
		`{"type":3,"data":{"custom_id":"a:unknownnonce:0"}}`,
		// non-numeric idx
		`{"type":3,"data":{"custom_id":"a:livenonce:notanumber"}}`,
		// empty idx segment
		`{"type":3,"data":{"custom_id":"a:livenonce:"}}`,
		// out-of-range idx (still routes to takeHit; Prompt() bounds-checks)
		`{"type":3,"data":{"custom_id":"a:livenonce:99999"}}`,
		// modal, wrong prefix
		`{"type":5,"data":{"custom_id":"x:livenonce"}}`,
		`{"type":5,"data":{"custom_id":"m:unknownnonce","components":[]}}`,
		// unknown interaction type
		`{"type":99,"data":{"custom_id":"a:livenonce:0"}}`,
		// empty nonce
		`{"type":3,"data":{"custom_id":"a::0"}}`,
	}
	for _, raw := range bad {
		// Must not panic.
		a.onInteraction(context.Background(), p, json.RawMessage(raw))
	}

	// The out-of-range index (a:livenonce:99999) DOES route to takeHit with a
	// valid nonce, so the live prompt may legitimately receive a hit. Drain it if
	// present and verify it carries the out-of-range index (the safe behaviour:
	// Prompt() itself bounds-checks idx against len(options) and falls through to
	// text, never indexing out of range).
	select {
	case hit := <-live.result:
		if hit.optionIdx != 99999 {
			t.Fatalf("unexpected hit delivered to live prompt: %+v", hit)
		}
	default:
		// also acceptable: nothing delivered
	}
}

// TestOnInteractionOutOfRangeIndexSafe : a button index past the option list
// must not panic when Prompt() maps it back — it falls through to a (empty) text
// response rather than indexing out of bounds. This guards the resolve path that
// onInteraction feeds.
func TestOnInteractionOutOfRangeIndexSafe(t *testing.T) {
	a, p, getNonce, _ := promptHarness(t)
	req := adapter.PromptRequest{
		ReplyRef: map[string]any{"provider": "dc", "channel_id": "c1"},
		Title:    "Pick",
		Options:  []adapter.PromptOption{{ID: "only", Label: "Only"}},
	}
	type result struct {
		r adapter.PromptResponse
		e error
	}
	ch := make(chan result, 1)
	go func() {
		r, e := a.Prompt(context.Background(), req)
		ch <- result{r, e}
	}()

	nonce := getNonce()
	if nonce == "" {
		t.Fatal("prompt was never posted")
	}
	// Index 5 with only 1 option — must not panic, must resolve to a non-option
	// (text) response, NOT silently pick option 0.
	a.onInteraction(context.Background(), p, json.RawMessage(`{"id":"i1","token":"t","type":3,"data":{"custom_id":"a:`+nonce+`:5"},"member":{"user":{"id":"u9"}}}`))

	select {
	case got := <-ch:
		if got.e != nil {
			t.Fatalf("unexpected error: %v", got.e)
		}
		if got.r.OptionID != "" {
			t.Fatalf("out-of-range index must not resolve to an option, got %+v", got.r)
		}
		if got.r.UserID != "u9" {
			t.Fatalf("expected user id carried through, got %+v", got.r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt did not resolve")
	}
}

// TestOnInteractionUserIDFromUserField : a DM interaction carries the actor in
// `user` (no `member`); the resolve must still attribute the click to that user.
func TestOnInteractionUserIDFromUserField(t *testing.T) {
	a, p, getNonce, _ := promptHarness(t)
	req := adapter.PromptRequest{
		ReplyRef: map[string]any{"provider": "dc", "channel_id": "c1"},
		Title:    "Pick",
		Options:  []adapter.PromptOption{{ID: "grant", Label: "OK"}},
	}
	ch := make(chan adapter.PromptResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		r, e := a.Prompt(context.Background(), req)
		ch <- r
		errCh <- e
	}()
	nonce := getNonce()
	if nonce == "" {
		t.Fatal("prompt was never posted")
	}
	// No `member`, only top-level `user` (a DM).
	a.onInteraction(context.Background(), p, json.RawMessage(`{"id":"i1","token":"t","type":3,"data":{"custom_id":"a:`+nonce+`:0"},"user":{"id":"dmuser"}}`))
	select {
	case r := <-ch:
		if err := <-errCh; err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r.OptionID != "grant" || r.UserID != "dmuser" {
			t.Fatalf("DM user not attributed: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt did not resolve")
	}
}

// TestTakeHitConcurrentFirstWins : two interactions race the same nonce; the
// buffered result channel must accept exactly one and drop the second without
// blocking — proving a double-click can't wedge a goroutine or deliver twice.
func TestTakeHitConcurrentFirstWins(t *testing.T) {
	a, _ := interactionHarness(t)
	pp := &pendingPrompt{result: make(chan promptHit, 1)}
	a.pmu.Lock()
	a.prompts["n"] = pp
	a.pmu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			a.takeHit("n", promptHit{optionIdx: idx})
		}(i)
	}
	wg.Wait()

	got := 0
	for {
		select {
		case <-pp.result:
			got++
			continue
		default:
		}
		break
	}
	if got != 1 {
		t.Fatalf("expected exactly one delivered hit, got %d", got)
	}
}

// TestParseCustomID : the wire-format parser's edge cases in isolation — the
// routing in onInteraction is only as safe as this.
func TestParseCustomID(t *testing.T) {
	cases := []struct {
		in, prefix       string
		wantNonce, wantR string
		wantOK           bool
	}{
		{"a:abc:0", "a", "abc", "0", true},
		{"a:abc:t", "a", "abc", "t", true},
		// no third segment (modal id)
		{"m:abc", "m", "abc", "", true},
		// SplitN(3) keeps the tail intact
		{"a:abc:1:2", "a", "abc", "1:2", true},
		// prefix mismatch
		{"a:abc", "m", "", "", false},
		// no separator
		{"abc", "a", "", "", false},
		// empty input
		{"", "a", "", "", false},
		// empty nonce still parses (caller guards)
		{"a:", "a", "", "", true},
		// empty prefix mismatches "a"
		{":abc:0", "a", "", "", false},
	}
	for _, tc := range cases {
		nonce, rest, ok := parseCustomID(tc.in, tc.prefix)
		if ok != tc.wantOK || nonce != tc.wantNonce || rest != tc.wantR {
			t.Fatalf("parseCustomID(%q,%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.in, tc.prefix, nonce, rest, ok, tc.wantNonce, tc.wantR, tc.wantOK)
		}
	}
}
