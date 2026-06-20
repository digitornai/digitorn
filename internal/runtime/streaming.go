package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/runtime/turn"
)

// chatOrStream invokes the LLM either synchronously or via
// streaming, depending on Engine.Streaming and whether the wired
// LLMChat satisfies the LLMStream contract. In streaming mode,
// each chunk's Delta is appended as an EventAssistantDelta event
// on the session bus so subscribers can render tokens live.
// The consolidated *llm.ChatResponse is returned to the agent
// loop with the same shape as the non-streaming path, so all
// downstream logic (tool dispatch, persistence) is identical.
//
// Errors during stream consumption are wrapped with "stream:"
// so the caller can distinguish them from synchronous failures
// at the error-message layer.
// chatOrStream wraps the LLM call with the daemon-wide concurrency semaphore
// (acquired per-call, released the instant the call returns — never held while
// an agent waits, which is what keeps nested delegation deadlock-free) and
// records real-time token telemetry to the ctx Recorder if one is attached.
func (e *Engine) chatOrStream(
	ctx context.Context, tr *turn.Turn, in TurnInput, req *llm.ChatRequest,
) (*llm.ChatResponse, error) {
	if e.LLMSem != nil {
		select {
		case e.LLMSem <- struct{}{}:
			defer func() { <-e.LLMSem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	resp, err := e.callLLM(ctx, tr, in, req)
	if err == nil && resp != nil {
		if r := RecorderFromContext(ctx); r != nil {
			r.AddLLMCall(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
			for range resp.ToolCalls {
				r.AddToolCall()
			}
		}
	}
	return resp, err
}

// callLLM performs the actual LLM call, synchronously or via streaming.
func (e *Engine) callLLM(
	ctx context.Context, tr *turn.Turn, in TurnInput, req *llm.ChatRequest,
) (*llm.ChatResponse, error) {
	if !e.Streaming {
		return e.LLM.Chat(ctx, req)
	}
	streamer, ok := e.LLM.(LLMStream)
	if !ok {
		// LLM client doesn't support streaming : fall back silently.
		// We don't return an error because the caller hasn't broken
		// any contract — streaming is best-effort.
		return e.LLM.Chat(ctx, req)
	}

	// Mark the message as streaming-mode at the start by setting
	// req.Stream=true. The worker uses this to pick the right
	// provider call shape ; the engine doesn't act on it
	// downstream.
	req.Stream = true
	// Instrument the split : the daemon's own work (load history, assemble
	// prompt, fire the call) is microseconds — measured at ~30ms from the event
	// log. callStart→first-token below is the time the agent actually waits, and
	// it is spent ENTIRELY upstream (engine→worker gRPC + worker→gateway +
	// provider prefill). Logging the TTFT makes "what blocks between my message
	// and the reply" answerable at a glance, every turn, no guessing.
	callStart := time.Now()
	chunks, err := streamer.ChatStream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("stream: open: %w", err)
	}

	var (
		contentBuilder   strings.Builder
		reasoningBuilder strings.Builder
		finalResp        = &llm.ChatResponse{}
		seenError        string
		liveOut          int  // running ~estimate of output tokens, for the live counter
		ttftLogged       bool // logged the provider time-to-first-token yet
	)
	// Per-index streaming state for tool calls : lets us surface the tool NAME
	// the instant it's detected and grow a token counter as the (possibly huge)
	// arguments stream in — so the client is never blind during a long write.
	liveTools := map[int]*liveToolCall{}
	var reasoningStartedAt, reasoningEndedAt int64 // wall-clock brackets for the thinking trace

	// Consume the stream, but bail the INSTANT the turn context is cancelled
	// (user abort). Without this select the loop would keep draining the
	// channel until the worker noticed the cancel — tokens kept flowing after
	// "stop". On abort we surface the partial content + the cancellation so the
	// caller can persist what was generated so far.
	for {
		select {
		case <-ctx.Done():
			finalResp.Content = contentBuilder.String()
			finalResp.ReasoningContent = reasoningBuilder.String()
			finalResp.ReasoningStartedAt = reasoningStartedAt
			finalResp.ReasoningEndedAt = reasoningEndedAt
			return finalResp, ctx.Err()
		case chunk, ok := <-chunks:
			if !ok {
				finalResp.Content = contentBuilder.String()
				finalResp.ReasoningContent = reasoningBuilder.String()
				finalResp.ReasoningStartedAt = reasoningStartedAt
				finalResp.ReasoningEndedAt = reasoningEndedAt
				// A non-empty seenError means the generation was cut off by a fault: the
				// gRPC consumer turns ANY non-EOF RecvMsg error (network drop, worker death,
				// deadline, upstream 5xx, rate limit) into an error chunk, so every failure
				// lands here carrying its partial, exactly like a user abort via ctx.Done().
				// A clean EOF (seenError == "") is a deliberate, complete end.
				if seenError != "" {
					return finalResp, fmt.Errorf("stream: provider error: %s", seenError)
				}
				return finalResp, nil
			}
			if chunk == nil {
				continue
			}
			if chunk.Error != "" {
				seenError = chunk.Error
				continue
			}
			// First content-bearing chunk : record the provider TTFT — the whole
			// wait between firing the call and the first token, spent upstream.
			if !ttftLogged && (chunk.Delta != "" || chunk.ReasoningDelta != "" ||
				len(chunk.ToolCallDeltas) > 0 || len(chunk.ToolCalls) > 0) {
				ttftLogged = true
				if e.Logger != nil {
					e.Logger.Info("runtime: llm first token",
						slog.Duration("provider_ttft", time.Since(callStart)),
						slog.String("model", req.Model),
						slog.Int("prompt_msgs", len(req.Messages)))
				}
			}
			if chunk.Delta != "" {
				contentBuilder.WriteString(chunk.Delta)
				// Running token estimate for the live counter (CTX-7.5) : a
				// cheap chars/4 add (nanoseconds), NEVER a tokeniser on the
				// streaming hot path. The exact total arrives via the round-end
				// usage anchor, which the client snaps to.
				liveOut += (len(chunk.Delta) + 3) / 4
				// Emit the delta on the durable bus so subscribers (Socket.IO
				// bridge, CLI streaming view) render tokens live. Best-effort :
				// a failed append never kills the turn.
				deltaEv := sessionstore.Event{
					Type:             sessionstore.EventAssistantDelta,
					SessionID:        in.SessionID,
					AppID:            in.AppID,
					UserID:           in.UserID,
					CorrelationID:    tr.ID,
					LiveOutputTokens: liveOut,
					Message: &sessionstore.MessagePayload{
						Role: "assistant",
						Parts: []sessionstore.MessagePart{
							{Type: sessionstore.PartTypeText, Text: chunk.Delta},
						},
					},
				}
				if _, err := e.Sessions.AppendDurable(ctx, deltaEv); err != nil {
					if e.Logger != nil {
						e.Logger.Warn("runtime: stream delta append failed", "err", err.Error())
					}
				}
			}
			// Accumulate the thinking-mode trace separately from the visible
			// content so it can be persisted on the assistant message and
			// replayed to providers that require it (DeepSeek thinking mode).
			// Also stream it LIVE so the client can render the agent's thinking
			// as it's generated. Transient (the projector ignores it) ; the
			// consolidated reasoning lands on the final assistant message.
			if chunk.ReasoningDelta != "" {
				if reasoningStartedAt == 0 {
					reasoningStartedAt = time.Now().UnixNano()
				}
				reasoningEndedAt = time.Now().UnixNano()
				reasoningBuilder.WriteString(chunk.ReasoningDelta)
				rev := sessionstore.Event{
					Type:          sessionstore.EventAssistantReasoningDelta,
					SessionID:     in.SessionID,
					AppID:         in.AppID,
					UserID:        in.UserID,
					CorrelationID: tr.ID,
					Message: &sessionstore.MessagePayload{
						Role:      "assistant",
						Reasoning: chunk.ReasoningDelta,
					},
				}
				if _, err := e.Sessions.AppendDurable(ctx, rev); err != nil && e.Logger != nil {
					e.Logger.Warn("runtime: reasoning delta append failed", "err", err.Error())
				}
			}
			// Surface streaming tool-call fragments : tool name (detected) +
			// a growing token counter, BEFORE the call is complete. Tool-call
			// ARGUMENTS are output tokens too — fold them into the SAME running
			// count as the text so the live counter reflects text + tools and a
			// finished tool's tokens never drop out of the total.
			if len(chunk.ToolCallDeltas) > 0 {
				for i := range chunk.ToolCallDeltas {
					liveOut += (chunk.ToolCallDeltas[i].ArgsChars + 3) / 4
				}
				e.emitToolCallStreamDeltas(ctx, tr, in, liveTools, chunk.ToolCallDeltas, liveOut)
			}
			if len(chunk.ToolCalls) > 0 {
				finalResp.ToolCalls = append(finalResp.ToolCalls, chunk.ToolCalls...)
			}
			if chunk.FinishReason != "" {
				finalResp.FinishReason = chunk.FinishReason
			}
			if chunk.Usage != nil {
				finalResp.Usage = *chunk.Usage
			}
		}
	}
}

// liveToolCall is the per-index accumulator for a streaming tool call : the id
// and name (taken from whichever fragment carries them) plus the running byte
// length of the arguments seen so far.
type liveToolCall struct {
	id           string
	name         string
	argChars     int
	emitted      bool // a streaming event was already sent for this call
	emittedChars int  // argChars at the last emitted event (throttle baseline)
}

// streamToolEmitChars throttles the streaming tool events : after the first
// frame (which shows the name), a new event is emitted only every this many
// characters of argument growth (~64 tokens). A streamed tool must NEVER flood
// the event bus — at one event per provider chunk a large write would emit
// hundreds of events, overflow the client's intake and make it drop real
// durable events (an assistant_message vanishing). Coarse live progress is
// plenty for a counter.
const streamToolEmitChars = 256

// emitToolCallStreamDeltas folds this chunk's tool-call fragments into the
// per-index live state and emits one EventToolCall (status="streaming") per
// touched call, so the client shows the tool NAME the instant it's detected and
// a token counter that grows while the (possibly huge) arguments stream in —
// the user is never blind during a long write. Display-only : the durable truth
// is still the final tool_call (pending) + tool_result. Best-effort ; a dropped
// frame costs nothing. Skipped until the call id is known (every provider sends
// it on the first fragment) so the client keys the chip stably and reconciles
// with the later pending event.
func (e *Engine) emitToolCallStreamDeltas(
	ctx context.Context, tr *turn.Turn, in TurnInput,
	live map[int]*liveToolCall, deltas []llm.ChatToolCallDelta, liveOut int,
) {
	for i := range deltas {
		d := &deltas[i]
		lt := live[d.Index]
		if lt == nil {
			lt = &liveToolCall{}
			live[d.Index] = lt
		}
		if d.ID != "" {
			lt.id = d.ID
		}
		if d.Name != "" {
			lt.name = d.Name
		}
		lt.argChars += d.ArgsChars
		if lt.id == "" || lt.name == "" {
			// Wait for BOTH the stable id AND the tool name before the first
			// frame : some providers send the id a fragment or two before the
			// function name, and emitting early made the client show a nameless
			// spinner ("⠴  …") until the call finished.
			continue
		}
		// Throttle : emit on the FIRST frame (show the name now), then only once
		// the args grew by streamToolEmitChars. Keeps the live counter moving
		// without flooding the bus.
		if lt.emitted && lt.argChars-lt.emittedChars < streamToolEmitChars {
			continue
		}
		lt.emitted = true
		lt.emittedChars = lt.argChars
		ev := sessionstore.Event{
			Type:          sessionstore.EventToolCall,
			SessionID:     in.SessionID,
			AppID:         in.AppID,
			UserID:        in.UserID,
			CorrelationID: tr.ID,
			// The running output-token count (text + tool args) so the client's
			// single live counter includes tool-call arguments, like assistant_delta.
			LiveOutputTokens: liveOut,
			Tool: &sessionstore.ToolPayload{
				CallID:     lt.id,
				Name:       lt.name,
				Status:     "streaming",
				LiveTokens: (lt.argChars + 3) / 4,
			},
		}
		if _, err := e.Sessions.AppendDurable(ctx, ev); err != nil && e.Logger != nil {
			e.Logger.Warn("runtime: tool_call stream delta append failed",
				slog.String("call_id", lt.id), slog.String("err", err.Error()))
		}
	}
}
