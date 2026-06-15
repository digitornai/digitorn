# Context Management Redesign — non-blocking, max-fidelity

## 0. Goal & principles

Make context management **ultra-reliable, performant, and NEVER blocking/slowing the agent loop**, while **preserving the maximum amount of context** for the LLM. We keep the real high-quality LLM summary (the "few-minute" compaction) but we do it **better than Claude Code**: that heavy summary runs **in the background, proactively**, so the loop never freezes. When the gate trips, we apply an **already-prepared** summary instantly; if none is ready, an instant local truncate unblocks the turn while the real summary catches up and swaps in on a later turn.

Principles:
1. **The live gauge is free** — driven by the provider `usage` (already captured), never an active recompute. (Keep.)
2. **The threshold is deterministic & local** — `window(model) − output_reserve − buffer`. (Keep, refine.)
3. **The loop NEVER waits on a summarizer LLM call.** The only inline work is O(µs–ms) local: micro-compaction + applying a ready summary, or a truncate.
4. **Maximum fidelity**: prefer the real LLM summary; micro-compaction elides bulk but keeps references; the background summary is incremental (summary-of-summary) and rich (decisions/files/state/open-threads).
5. **Build on what exists** — reuse the gauge, tracker, recount pool, the `CutoffSeq`/`ApplyView` view-rebuild, and the event-sourced projection. Don't rewrite; extend.

## 1. What exists (reuse map)

| Capability | Where | Reuse |
|---|---|---|
| Provider `usage` capture (exact, free) | `llm.Usage` (`internal/llm/types.go:245`); `bifrost/service.go:604-622` (resp) + `:663-676` (stream); `streaming.go:213`; `engine.emitTokenUsage` (`engine.go:1703`) → `EventTokenUsage` | **Keep as the gauge.** |
| Live gauge / Tracker (lock-free) | `contextsvc.Tracker` (`contextview.go:166`), `freshContextView` (`engine.go`), `ContextProviderTokens` (`state.go:94`) | Keep. |
| Deterministic threshold | window table `contextcompact/windows.go:15`; `resolveAutoCompact` (`engine.go:1610`); `used/limit ≥ 0.97` | Keep; add an absolute buffer term. |
| Background recount pool | `contextsvc.Background` (`background.go`), `Touch`/coalesce/bounded pool | **Mirror** for the summary maintainer. |
| Compaction view rebuild | `CutoffSeq` + `ApplyView(msgs, cutoffSeq, summary)` (`compact.go:347`); `ContextCompactionPayload.Summary` | **This is the session-memory store already.** Reuse. |
| Summary + truncate strategies | `Summarize` (`compact.go:160`, LLM), `Truncate` (`compact.go:135`, instant), `SafeSplitIndex` (`compact.go:61`, orphan-tool-result guard) | Reuse; move `Summarize` OFF the loop. |
| Compactor entry | `Compactor.CompactSession(ctx, sid, strategy, keepLast)` (`bootstrap.go:655`) | Extend with an "apply-prepared / never-call-LLM-inline" path. |

## 2. The three problems (today)

1. **🔴 Synchronous compaction on the loop** — `guardContextPressure` (`engine.go:1045` + `:1650`) runs every round; on pressure it calls `CompactSession` **inline**, and `strategy:summarize` makes an **in-band LLM call** (seconds). Also `enforcePromptBudget` (`engine.go:1527`) and emergency overflow retry (`engine.go:1124`). **This is the blocking we must remove.**
2. **🟠 `BuildFor()` first-call `sync.Once`** (`wiring/builder.go:167`) — semantic attach (embeddings network) blocks concurrent identical first requests 10-100ms.
3. **🟡 Background recount O(N)** — `contextViewSource.ContextView` reloads `sessionStore.State()` + iterates all messages + 3 separate tokenizer RPCs; saturates the 4-worker pool on large/heavy sessions.

## 3. Architecture — the non-blocking cascade

```
                       ┌──────────── HOT PATH (turn loop) ─────────────┐
 provider usage  ───►  │  live gauge (free, exact)  ──► threshold gate │
 (EventTokenUsage)     │                                   │           │
                       │              ┌────────────────────┘           │
                       │   pressure ≥ threshold?                       │
                       │       │ no → proceed                          │
                       │       │ yes:                                  │
                       │       │   1. micro-compact (inline, no LLM)   │  Phase 2
                       │       │   2. prepared summary ready & covers? │
                       │       │        yes → apply it (instant)       │  Phase 1
                       │       │        no  → truncate-only (instant)  │  Phase 1
                       │       │             + mark "summary urgent"   │
                       │       └─► build prompt on the reduced view    │
                       └───────────────────────────────────────────────┘
                                       │ Touch(sid)           ▲ apply ready summary
                                       ▼                      │ next turn
            ┌──────────── BACKGROUND (off loop) ──────────────────────────┐
            │  Summary Maintainer (coalesced, bounded pool)               │
            │   • compute aged region (msgs older than the moving boundary)│
            │   • incremental LLM summary (prior summary + newly aged)     │  Phase 3
            │     — rich: decisions / files / state / open threads         │
            │     — may take "minutes"; that's fine, it's off the loop     │
            │   • persist EventContextSummaryPrepared{summary, coversSeq}  │
            │  Recount pool (exists) — keeps the gauge breakdown fresh     │
            └──────────────────────────────────────────────────────────────┘
```

### 3.1 Live gauge & threshold (Phase 0 — keep, minor refine)
- Gauge stays the provider `usage` anchor (free, exact). The gate compares `max(trackerUsed, lastPromptTokens)` to `limit`.
- Refine the threshold to mirror a Claude-style absolute buffer in addition to the ratio: trip when `used ≥ limit − BUFFER` **or** `used/limit ≥ ratio`, whichever is lower. `BUFFER` model-scaled (e.g. ~13k at 200k, more at 1M). Deterministic, local, recomputed per turn at zero cost.

### 3.2 Instant gate (Phase 1 — the critical fix)
Replace the inline `Summarize` in `guardContextPressure`/`enforcePromptBudget` with `applyCompaction(sid, prepared)`:
```
applyCompaction(sid):
  view = freshView(sid)
  if !overThreshold(view): return notCompacted
  microCompact(sid)                         # Phase 2 (inline, no LLM); Phase 1: no-op
  if stillOver(sid):
    prep = sessionState.PreparedSummary      # set by the background maintainer
    if prep != nil && prep.CoversSeq >= neededCutoff(sid):
        emit EventContextCompacted{ CutoffSeq: prep.CoversSeq, Summary: prep.Summary,
                                    Strategy: "summarize", source: "prepared" }   # INSTANT
    else:
        cut = SafeSplitIndex(msgs, conservativeKeep)        # keep MORE when no summary
        emit EventContextCompacted{ CutoffSeq: cut, Summary: deterministicRecap,
                                    Strategy: "truncate", source: "fallback" }     # INSTANT
        touchSummaryUrgent(sid)              # background produces real summary → swaps in next turn
  reload snap from the compacted view
```
**No branch calls the summarizer LLM.** Worst case inline cost = `SafeSplitIndex` + a durable event append (µs–low-ms). The loop never freezes.

### 3.3 Background Summary Maintainer (Phase 1 + 3)
A new service mirroring `contextsvc.Background` (coalesced `Touch`, bounded pool, per-session dedup), wired in bootstrap next to the recount worker. On `Touch(sid)`:
1. Load state; compute the **aged region** = messages below a moving boundary (older than `keepRecent` AND/OR the region whose tokens exceed a fraction of the window). Skip if the aged region hasn't grown enough since the last prepared summary (hysteresis → avoids churn).
2. Run the **incremental** summary via the existing `Summarizer`: `priorSummary + newlyAgedMessages → newSummary`. Bounded input (only the delta), so each pass is cheap-ish; quality is high (Phase 3 prompt).
3. Persist `EventContextSummaryPrepared{ Summary, CoversSeq, InputTokens, Model }` (coalesced, non-durable write like the recount event). Projection sets `SessionState.PreparedSummary{Summary, CoversSeq}`.
4. Proactive cadence: triggered by the same context-growth touches that drive the recount, but rate-limited (e.g. at most one summary pass per session per N seconds, and only when the aged region grew by ≥ M tokens). "Urgent" touches (from a truncate fallback) jump the queue.

This is the session-memory: the real LLM summary is **always being kept ready ahead of the gate.** Because it's off the loop, it can be as thorough as needed.

### 3.4 Micro-compaction (Phase 2)
Inline, no LLM, lossless-ish: within the kept view, elide the *body* of stale/bulky tool results (older than the last few rounds, above a byte threshold) replacing them with a compact reference (`[tool_result elided: <tool>, <n> bytes, seq <s>]`) while keeping the tool_call/tool_result pairing intact. Recovers space without dropping structure or needing a summary. Runs first in the gate; often defers heavy compaction entirely.

### 3.5 Quality / max fidelity (Phase 3)
- Summary prompt structured to preserve maximum signal: **task/goal, decisions made, files & artifacts touched, current state, open threads / TODOs, key facts & constraints, and a terse running narrative**. Not a lossy paragraph.
- Incremental (summary-of-summary + newly-aged) so old detail isn't re-lost and the summary stays bounded.
- The cascade prefers the prepared LLM summary; truncate is only the never-block stopgap, and keeps `conservativeKeep` (more recent messages) to minimise loss until the summary swaps in.

### 3.6 Hardening (Phase 4)
- Cache message plaintext at append time (kill the O(N) re-iteration in recount + maintainer).
- Batch the 3 tokenizer RPCs (system/tools/messages) into one call.
- Pre-warm `BuildFor` at app install / defer semantic attach to first real use so first-request never blocks.

## 4. New events / state (Phase 1)
- `EventContextSummaryPrepared` (coalesced) → `ContextSummaryPayload{ Summary string, CoversSeq uint64, InputTokens int, Model string }`.
- `SessionState.PreparedSummary *PreparedSummary{ Summary string, CoversSeq uint64 }` (projection, last-value-wins; cleared/advanced on `EventContextCompacted`).
- Reuse `EventContextCompacted` + `CutoffSeq` + `ApplyView` unchanged for the actual applied view.

## 5. Risks & test plan
- **Hot-path regression (engine loop is sensitive).** Mitigate: gate change is additive (new `source` path); the inline-LLM call is simply never reached. Keep `Truncate` as the guaranteed instant fallback. Feature-flag the maintainer; if off, behaviour = today minus the inline summary (truncate-only).
- **Summary staleness at the gate** (prepared summary lags a fast balloon): the truncate fallback covers it; the maintainer's urgent path swaps the real summary in next turn. Test: balloon context faster than the maintainer → assert the turn is NOT blocked and a real summary appears within K turns.
- **Orphan tool_result** when applying a prepared summary at `CoversSeq`: the maintainer must choose `CoversSeq` via `SafeSplitIndex` semantics so the kept view is self-consistent. Test from `compact_test.go`.
- **Determinism/resume**: `EventContextCompacted` (durable) still reproduces the view on reload; `PreparedSummary` is a coalesced hint (rebuildable), not durable-critical.
- Tests: gate never invokes the summarizer (inject a summarizer that panics if called on the loop); maintainer produces/advances the prepared summary; instant-apply path; conservative-truncate fallback; non-blocking under a synthetic slow summarizer (assert turn latency unaffected).

## 6. Phase plan
1. **Phase 1 — kill the blocking.** Background Summary Maintainer (incremental, off-loop) + `EventContextSummaryPrepared`/`PreparedSummary` + instant gate (apply-prepared / conservative-truncate, never inline-LLM). Prove: a slow summarizer never adds turn latency.
2. **Phase 2 — micro-compaction** inline (tool-result elision with references).
3. **Phase 3 — fidelity** (rich incremental summary prompt + structure; prefer-summary cascade).
4. **Phase 4 — perf hardening** (plaintext cache, RPC batch, BuildFor pre-warm).
