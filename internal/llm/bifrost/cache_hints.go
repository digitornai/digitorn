// Package bifrost — prompt-caching strategy applied uniformly to every
// outbound request. The daemon marks "stable prefix" breakpoints with
// llm.CacheControl{Type: "ephemeral"}; downstream:
//
//   - Anthropic / Bedrock-Claude / Vertex-Claude → cache hit on the
//     marked prefix (5-min TTL, ~90% cost reduction, ~5× TTFT speed-up).
//   - OpenAI / DeepSeek / Azure-OpenAI → auto-caching kicks in on
//     stable prefixes ≥1024 tokens; the gateway-go strips our markers
//     since these providers don't accept Anthropic-shape hints.
//   - Mistral / Cohere / Groq → no provider caching; markers stripped,
//     no-op.
//   - Gemini → explicit cachedContents API not driven from here yet.
//
// Hence: ONE code path in the daemon, NO provider branching. The
// gateway and Bifrost handle dispatch.
//
// Strategy = AGGRESSIVE (up to 4 breakpoints), validated by the user:
//
//	#1 — last system message      (cache "system only")
//	#2 — last tool definition     (cache "system + tools")
//	#3 — pre-recent history       (cache "system + tools + stable history",
//	                               i.e. everything before the last 2 turns)
//	#4 — deep mid-conversation    (cache "system + tools + early history",
//	                               only for conversations >10 messages)
//
// Anthropic LRUs the 4 cache entries, so later calls with slight prefix
// drift still hit one of the older anchors.
package bifrost

import (
	"strings"

	"github.com/mbathepaul/digitorn/internal/llm"
)

// recapSentinel marks a compaction handoff system message (the runtime wraps the
// recap in <recap>…</recap>). Such a message is VOLATILE — it changes on every
// compaction, so a cache breakpoint on it never hits — and marking it cacheable
// forces the richer content-block form, which some providers treat differently
// and then stop surfacing to the user. So the system cache breakpoint skips it
// and anchors on the stable prefix instead.
const recapSentinel = "<recap>"

func isVolatileRecap(m *llm.ChatMessage) bool {
	return m.Role == "system" && strings.Contains(m.Content, recapSentinel)
}

// ephemeralCC is the singleton "ephemeral" marker. Pointer reused
// everywhere — zero per-call allocation. The pointer is read-only from
// every call site (we never mutate it), so sharing it is race-safe.
var ephemeralCC = &llm.CacheControl{Type: "ephemeral"}

const (
	// recentTurnsKeptUncached: messages in the "recent" tail that we do
	// NOT mark as cache anchors. Anything before these is fair game for
	// the breakpoint #3. Two turns = current user message + the
	// assistant turn before it (the one whose follow-up they typed).
	recentTurnsKeptUncached = 2

	// deepAnchorThreshold: history length below which we don't bother
	// with the breakpoint #4 — the conversation is too short to benefit
	// from a "deep" cache anchor in addition to breakpoint #3.
	deepAnchorThreshold = 10
)

// markStablePrefixCacheable applies up to 4 Anthropic cache breakpoints
// on the request's stable prefix. Idempotent — re-runs simply re-mark
// the same positions (or no-op if nothing has changed).
//
// Returns the number of breakpoints actually applied — useful for
// tests and metrics. Always ≤ 4 (Anthropic hard cap).
func markStablePrefixCacheable(req *llm.ChatRequest) int {
	if req == nil || len(req.Messages) == 0 {
		return 0
	}

	used := 0

	// --- Breakpoint #1 : last system message ---
	// Many agents have one big system prompt at the top; some apps
	// append tool/persona hints mid-conversation. We anchor on the
	// LAST system msg because anything before it is also covered by
	// cache transitivity (the prefix is everything up to that point).
	lastSysIdx := -1
	for i := range req.Messages {
		if req.Messages[i].Role == "system" && !isVolatileRecap(&req.Messages[i]) {
			lastSysIdx = i
		}
	}
	if lastSysIdx >= 0 {
		markMessageCacheable(&req.Messages[lastSysIdx])
		used++
	}

	// --- Breakpoint #2 : last tool definition ---
	// Tools list is sent as a top-level array; marking the LAST tool
	// caches the whole array. Most apps reuse identical tool defs
	// across calls — high hit-rate target.
	if n := len(req.Tools); n > 0 {
		req.Tools[n-1].CacheControl = ephemeralCC
		used++
	}

	// --- Breakpoint #3 : pre-recent history boundary ---
	// Mark the message just before the last `recentTurnsKeptUncached`
	// messages, so the stable history (everything except the current
	// user question and its preceding assistant reply) is cached.
	// Skip if the conversation isn't long enough OR if we'd overlap
	// with the system message we already marked.
	if used < 4 && len(req.Messages) > recentTurnsKeptUncached+1 {
		anchor := len(req.Messages) - 1 - recentTurnsKeptUncached
		if anchor > lastSysIdx {
			markMessageCacheable(&req.Messages[anchor])
			used++
		}
	}

	// --- Breakpoint #4 : deep mid-history anchor ---
	// For longer conversations, add an extra anchor near the middle
	// so that as the chat grows, the breakpoint #3 anchor moving
	// forward doesn't lose us early-history cache entirely (LRU
	// eviction). The middle is "(lastSys+1 .. anchor3-1) / 2".
	if used < 4 && len(req.Messages) > deepAnchorThreshold {
		anchor3 := len(req.Messages) - 1 - recentTurnsKeptUncached
		// Mid-point between system end and breakpoint #3 anchor.
		midStart := lastSysIdx + 1
		if midStart < 0 {
			midStart = 0
		}
		mid := midStart + (anchor3-midStart)/2
		// Only place it if it doesn't overlap any earlier anchor.
		if mid > lastSysIdx && mid < anchor3 {
			markMessageCacheable(&req.Messages[mid])
			used++
		}
	}

	return used
}

// markMessageCacheable applies cache_control to the right spot of a
// ChatMessage. Anthropic expects the marker on the LAST content block
// when Parts is used, or on the message itself when Content (string)
// is set.
//
// We pick the most-specific available location:
//
//  1. Parts non-empty → mark LAST block of Parts
//  2. Otherwise → mark on the message
func markMessageCacheable(m *llm.ChatMessage) {
	if m == nil || isVolatileRecap(m) {
		return
	}
	if n := len(m.Parts); n > 0 {
		m.Parts[n-1].CacheControl = ephemeralCC
		return
	}
	m.CacheControl = ephemeralCC
}

// needsContentBlocks reports whether buildChatRequest must switch this
// message from the cheap string `content` form to the typed
// ChatContentBlock array — required when ANY cache hint is present
// (Bifrost reads cache_control off blocks, not the string), or when the
// caller already supplied Parts.
func needsContentBlocks(m *llm.ChatMessage) bool {
	if m == nil {
		return false
	}
	if m.CacheControl != nil {
		return true
	}
	for i := range m.Parts {
		if m.Parts[i].CacheControl != nil {
			return true
		}
	}
	return len(m.Parts) > 0
}

