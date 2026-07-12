package bifrost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	schemas "github.com/maximhq/bifrost/core/schemas"
	"golang.org/x/sync/semaphore"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/digitornai/digitorn/internal/llm"
)

// llmRequestLog is set once at startup. Set DIGITORN_LOG_LLM_REQUEST=1 to
// dump every outgoing chat request (messages, tools, system) to stderr.
var llmRequestLog = os.Getenv("DIGITORN_LOG_LLM_REQUEST") == "1"

func logLLMRequest(req *llm.ChatRequest, _ *schemas.BifrostChatRequest) {
	if !llmRequestLog {
		return
	}
	type msgLine struct {
		Role  string `json:"role"`
		Chars int    `json:"chars"`
		Snip  string `json:"snippet"`
	}
	var msgs []msgLine
	totalChars := 0
	for _, m := range req.Messages {
		chars := len(m.Content)
		totalChars += chars
		snip := m.Content
		if len(snip) > 120 {
			snip = snip[:120] + "…"
		}
		msgs = append(msgs, msgLine{m.Role, chars, snip})
	}
	out := map[string]any{
		"model":        req.Model,
		"provider":     req.Provider,
		"byok":         req.BYOK,
		"n_tools":      len(req.Tools),
		"n_messages":   len(req.Messages),
		"total_chars":  totalChars,
		"approx_tokens": totalChars / 4,
		"messages":     msgs,
	}
	if len(req.Tools) > 0 {
		toolNames := make([]string, 0, len(req.Tools))
		toolChars := 0
		for _, t := range req.Tools {
			toolNames = append(toolNames, t.Name)
			b, _ := json.Marshal(t)
			toolChars += len(b)
		}
		out["tool_names"] = toolNames
		out["tools_chars"] = toolChars
		out["tools_approx_tokens"] = toolChars / 4
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintf(os.Stderr, "\n[LLM_REQUEST]\n%s\n", b)
}

// Service implements llm.Service by delegating to a Bifrost client.
type Service struct {
	client  *bifrost.Bifrost
	cfg     Config
	plugins *PluginSet

	// admission bounds concurrent in-flight requests to BufferSize and
	// returns codes.ResourceExhausted on overflow. Without it, Bifrost
	// silently drops above InitialPoolSize (DropExcessRequests=true).
	// Acquire respects ctx, so a request waiting in the queue cancels
	// cleanly when its own ctx expires.
	admission *semaphore.Weighted
}

func NewService(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 256
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 16384
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	plugins := NewPluginSet(logger, cfg.AuditEnabled, cfg.CBThreshold, cfg.CBWindow, cfg.CBOpenFor)
	acc := newAccount(cfg)
	client, err := bifrost.Init(ctx, schemas.BifrostConfig{
		Account:            acc,
		LLMPlugins:         plugins.AsLLMPlugins(),
		InitialPoolSize:    cfg.BufferSize,
		DropExcessRequests: true,
	})
	if err != nil {
		return nil, fmt.Errorf("bifrost init: %w", err)
	}
	return &Service{
		client:    client,
		cfg:       cfg,
		plugins:   plugins,
		admission: semaphore.NewWeighted(int64(cfg.BufferSize)),
	}, nil
}

// admit acquires one admission slot honouring ctx cancellation. Returns
// codes.ResourceExhausted when the slot can't be acquired before ctx
// expires. Hot path = ~10 ns when slots are free.
func (s *Service) admit(ctx context.Context) error {
	if err := s.admission.Acquire(ctx, 1); err != nil {
		return status.Errorf(codes.ResourceExhausted,
			"llm worker admission denied: %v (in-flight=%d/%d)",
			err, 0, s.cfg.BufferSize)
	}
	return nil
}

// Plugins exposes the plugin set so the worker binary can dump stats.
func (s *Service) Plugins() *PluginSet { return s.plugins }

func (s *Service) Shutdown() { s.client.Shutdown() }

// ---- llm.Service implementation ----

func (s *Service) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	if err := s.admit(ctx); err != nil {
		return nil, err
	}
	defer s.admission.Release(1)
	bctx, cancel, route := s.buildContext(ctx, req)
	defer cancel()
	defer releaseRouteInfo(route)
	breq := s.buildChatRequest(req)
	logLLMRequest(req, breq)
	resp, berr := s.client.ChatCompletionRequest(bctx, breq)
	if berr != nil {
		ec := errCtxForChat(req)
		s.logBifrostError(berr, ec, "chat")
		return nil, translateError(berr, ec)
	}
	return mapChatResponse(resp), nil
}

func (s *Service) ChatStream(ctx context.Context, req *llm.ChatRequest, sink llm.ChatStreamSink) error {
	if err := s.admit(ctx); err != nil {
		return err
	}
	defer s.admission.Release(1)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bctx, bcancel, route := s.buildContext(ctx, req)
	defer bcancel()
	defer releaseRouteInfo(route)
	breq := s.buildChatRequest(req)
	logLLMRequest(req, breq)
	// Gateway TTFT : the worker side of the split. Compared with the engine-side
	// provider_ttft this isolates the engine→worker gRPC IPC (their tiny
	// difference) from the gateway+provider prefill (this number) — so "is it the
	// gateway?" is answered with data, not a hunch.
	gwStart := time.Now()
	stream, berr := s.client.ChatCompletionStreamRequest(bctx, breq)
	if berr != nil {
		ec := errCtxForChat(req)
		s.logBifrostError(berr, ec, "chat_stream_init")
		return translateError(berr, ec)
	}
	acc := newToolCallAccumulator()
	ecStream := errCtxForChat(req)
	firstSent := false
	for chunk := range stream {
		// Mid-stream errors: log with the same structured shape as the
		// initial-dispatch failure so dashboards can see "the upstream
		// hung up at chunk 47" without trawling raw bytes. The chunk's
		// `Error` field still propagates downstream verbatim (no
		// behaviour change) — this is observability only.
		if chunk != nil && chunk.BifrostError != nil {
			s.logBifrostError(chunk.BifrostError, ecStream, "chat_stream_mid")
		}
		// Accumulate any tool_call fragments this chunk carries — they
		// are incremental and only usable once merged by index.
		acc.add(rawDeltaToolCalls(chunk))

		out := mapChatChunk(chunk)
		if out == nil {
			continue
		}
		// Skip empty carrier chunks. A tool_call-fragment chunk is NOT empty
		// anymore : its fragments are surfaced via ToolCallDeltas so the client
		// can render the streaming call, so keep those.
		if out.Delta == "" && out.ReasoningDelta == "" && out.FinishReason == "" &&
			out.Usage == nil && out.Error == "" && len(out.ToolCallDeltas) == 0 {
			continue
		}
		if err := sink.Send(out); err != nil {
			return err
		}
		if !firstSent {
			firstSent = true
			if lg := s.cfg.Logger; lg != nil {
				lg.Info("bifrost: gateway first token",
					slog.Duration("gateway_ttft", time.Since(gwStart)),
					slog.String("model", req.Model))
			}
		}
	}
	// Flush the merged tool calls as a final chunk so the engine's
	// stream consumer sees complete, decoded calls exactly once.
	if merged := acc.merged(); len(merged) > 0 {
		if err := sink.Send(&llm.ChatChunk{ToolCalls: merged}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Embed(ctx context.Context, req *llm.EmbedRequest) (*llm.EmbedResponse, error) {
	if err := s.admit(ctx); err != nil {
		return nil, err
	}
	defer s.admission.Release(1)
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	route := acquireRouteInfo(req.BYOK, req.APIKey, req.UserJWT, req.BaseURL)
	defer releaseRouteInfo(route)
	bc := schemas.NewBifrostContextWithValue(bctx, time.Time{}, ctxKeyRoute, route)

	resp, berr := s.client.EmbeddingRequest(bc, &schemas.BifrostEmbeddingRequest{
		Provider: ResolveProvider(&llm.ChatRequest{
			BYOK:     req.BYOK,
			Provider: req.Provider,
		}),
		Model: req.Model,
		Input: &schemas.EmbeddingInput{Texts: req.Input},
	})
	if berr != nil {
		ec := errCtxForEmbed(req)
		s.logBifrostError(berr, ec, "embed")
		return nil, translateError(berr, ec)
	}
	// Preallocate Embeddings capacity from response length — eliminates
	// 1-2 reallocations on typical embed batch sizes (≥4 inputs).
	out := &llm.EmbedResponse{
		Model:      resp.Model,
		Embeddings: make([][]float64, 0, len(resp.Data)),
	}
	for _, e := range resp.Data {
		if e.Embedding.EmbeddingArray != nil {
			out.Embeddings = append(out.Embeddings, e.Embedding.EmbeddingArray)
		}
	}
	if resp.Usage != nil {
		out.Usage = llm.Usage{
			PromptTokens: resp.Usage.PromptTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		}
	}
	return out, nil
}

func (s *Service) CountTokens(ctx context.Context, req *llm.CountTokensRequest) (*llm.CountTokensResponse, error) {
	// Bifrost's CountTokens takes a ResponsesRequest. For V1 we approximate
	// by serving a basic estimate ; the worker's plugin can later wire the
	// real provider-specific token counter.
	return &llm.CountTokensResponse{
		Model:  req.Model,
		Tokens: uint64(estimateTokens(req.Messages)),
	}, nil
}

func (s *Service) ListProviders(ctx context.Context, _ *llm.ListProvidersRequest) (*llm.ListProvidersResponse, error) {
	names := [...]string{
		"anthropic", "openai", "azure", "bedrock", "cohere", "mistral",
		"gemini", "vertex", "groq", "fireworks", "perplexity", "cerebras",
		"ollama", "vllm", "openrouter", "huggingface", "xai", "sgl",
		"nebius", "parasail", "replicate", "digitorn",
	}
	out := &llm.ListProvidersResponse{Providers: make([]llm.ProviderInfo, 0, len(names))}
	for _, name := range names {
		out.Providers = append(out.Providers, llm.ProviderInfo{
			Name:         name,
			Modalities:   []string{"chat", "embed"},
			GatewayReady: name == "digitorn",
		})
	}
	return out, nil
}

// Speak synthesizes text to streamed audio through the gateway (bifrost
// SpeechStreamRequest). Audio deltas are forwarded as raw frames the instant they
// arrive — the latency-critical "time to first audio" path — terminated by a Done
// frame. Routing, admission, and error translation mirror Chat.
func (s *Service) Speak(ctx context.Context, req *llm.SpeechRequest, sink llm.AudioSink) error {
	if err := s.admit(ctx); err != nil {
		return err
	}
	defer s.admission.Release(1)
	bctx, cancel, route := s.buildAudioContext(ctx, audioRoute{
		BYOK: req.BYOK, APIKey: req.APIKey, UserJWT: req.UserJWT, BaseURL: req.BaseURL,
		Timeout: req.Timeout, CorrelationID: req.CorrelationID,
		AppID: req.AppID, SessionID: req.SessionID, UserID: req.UserID, AgentID: req.AgentID,
	})
	defer cancel()
	defer releaseRouteInfo(route)

	var voice *schemas.SpeechVoiceInput
	if req.Voice != "" {
		v := req.Voice
		voice = &schemas.SpeechVoiceInput{Voice: &v}
	}
	sp := &schemas.BifrostSpeechRequest{
		Provider: ResolveProvider(&llm.ChatRequest{BYOK: req.BYOK, Provider: req.Provider}),
		Model:    req.Model,
		Input:    &schemas.SpeechInput{Input: req.Text},
		Params: &schemas.SpeechParameters{
			VoiceConfig:    voice,
			ResponseFormat: req.Format,
			Speed:          req.Speed,
			Instructions:   req.Instructions,
		},
	}
	ec := errCallContext{Provider: req.Provider, Model: req.Model, BYOK: req.BYOK,
		CorrelationID: req.CorrelationID, SessionID: req.SessionID, UserID: req.UserID, AgentID: req.AgentID}

	// The digitorn gateway's /v1/audio/speech returns the full audio in one
	// response (non-SSE), so use the unary SpeechRequest and chunk the result into
	// frames for smooth downstream playback.
	resp, berr := s.client.SpeechRequest(bctx, sp)
	if berr != nil {
		s.logBifrostError(berr, ec, "speak")
		return translateError(berr, ec)
	}
	if resp != nil {
		audio := resp.Audio
		const chunk = 9600 // ~200 ms of PCM16 @ 24 kHz
		for off := 0; off < len(audio); off += chunk {
			end := off + chunk
			if end > len(audio) {
				end = len(audio)
			}
			if err := sink.Send(llm.AudioBytesFrame(audio[off:end])); err != nil {
				return err
			}
		}
	}
	return sink.Send(llm.DoneFrame())
}

// Transcribe turns an utterance's audio into streamed transcript frames through the
// gateway (bifrost TranscriptionStreamRequest). The utterance is VAD-delimited by the
// caller, so the request carries a bounded audio buffer; delta events are forwarded as
// TextFrame (interim) and the done event as a FinalFrame.
func (s *Service) Transcribe(ctx context.Context, req *llm.TranscribeRequest, sink llm.AudioSink) error {
	if err := s.admit(ctx); err != nil {
		return err
	}
	defer s.admission.Release(1)
	bctx, cancel, route := s.buildAudioContext(ctx, audioRoute{
		BYOK: req.BYOK, APIKey: req.APIKey, UserJWT: req.UserJWT, BaseURL: req.BaseURL,
		Timeout: req.Timeout, CorrelationID: req.CorrelationID,
		AppID: req.AppID, SessionID: req.SessionID, UserID: req.UserID, AgentID: req.AgentID,
	})
	defer cancel()
	defer releaseRouteInfo(route)

	params := &schemas.TranscriptionParameters{}
	if req.Language != "" {
		l := req.Language
		params.Language = &l
	}
	if req.Format != "" {
		f := req.Format
		params.Format = &f
	}
	tr := &schemas.BifrostTranscriptionRequest{
		Provider: ResolveProvider(&llm.ChatRequest{BYOK: req.BYOK, Provider: req.Provider}),
		Model:    req.Model,
		Input:    &schemas.TranscriptionInput{File: req.Audio, Filename: "utterance." + audioExt(req.Format)},
		Params:   params,
	}
	ec := errCallContext{Provider: req.Provider, Model: req.Model, BYOK: req.BYOK,
		CorrelationID: req.CorrelationID, SessionID: req.SessionID, UserID: req.UserID, AgentID: req.AgentID}

	// The gateway's /v1/audio/transcriptions returns the full transcript in one
	// (non-SSE) response, so use the unary TranscriptionRequest.
	resp, berr := s.client.TranscriptionRequest(bctx, tr)
	if berr != nil {
		s.logBifrostError(berr, ec, "transcribe")
		return translateError(berr, ec)
	}
	if resp != nil && resp.Text != "" {
		if err := sink.Send(llm.FinalFrame(resp.Text)); err != nil {
			return err
		}
	}
	return sink.Send(llm.DoneFrame())
}

// audioExt maps a wire format name to a filename extension (providers infer the
// codec from it). Defaults to wav for unknown/empty formats.
func audioExt(format string) string {
	switch format {
	case "", "pcm", "pcm16", "l16", "wav":
		return "wav"
	case "mp3", "ogg", "flac", "opus", "webm", "m4a":
		return format
	case "mulaw", "ulaw", "g711":
		return "wav"
	default:
		return "wav"
	}
}

// audioRoute carries the routing + identity fields common to the audio RPCs.
type audioRoute struct {
	BYOK    bool
	APIKey  string
	UserJWT string
	BaseURL string
	Timeout time.Duration

	CorrelationID string
	AppID         string
	SessionID     string
	UserID        string
	AgentID       string
}

// buildAudioContext mirrors buildContext for the audio RPCs: pooled route info,
// gateway passthrough when not BYOK, and per-request trace identity.
func (s *Service) buildAudioContext(parent context.Context, r audioRoute) (*schemas.BifrostContext, context.CancelFunc, *routeInfo) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	bc, cancel := schemas.NewBifrostContextWithTimeout(parent, timeout)
	route := acquireRouteInfo(r.BYOK, r.APIKey, r.UserJWT, r.BaseURL)
	bc.SetValue(ctxKeyRoute, route)
	if !r.BYOK {
		bc.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
	}
	if r.CorrelationID != "" {
		bc.SetTraceAttribute("correlation_id", r.CorrelationID)
	}
	if r.SessionID != "" {
		bc.SetTraceAttribute("session_id", r.SessionID)
	}
	if r.UserID != "" {
		bc.SetTraceAttribute("user_id", r.UserID)
	}
	if r.AgentID != "" {
		bc.SetTraceAttribute("agent_id", r.AgentID)
	}
	// Same gateway attribution as buildContext — audio calls are billed too.
	if !r.BYOK {
		setAttributionHeaders(bc, r.AppID, r.SessionID, r.AgentID, r.CorrelationID)
	}
	return bc, cancel, route
}

// ---- translation helpers ----

func (s *Service) buildContext(parent context.Context, req *llm.ChatRequest) (*schemas.BifrostContext, context.CancelFunc, *routeInfo) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 600 * time.Second
	}
	bc, cancel := schemas.NewBifrostContextWithTimeout(parent, timeout)
	// Single ctx.Value entry : keeps the lookup chain at depth 1 inside
	// Bifrost's account callbacks (O(1), one map probe). The routeInfo
	// is pooled — the caller MUST releaseRouteInfo(route) after the
	// request completes.
	route := acquireRouteInfo(req.BYOK, req.APIKey, req.UserJWT, req.BaseURL)
	bc.SetValue(ctxKeyRoute, route)
	applyProviderProtocol(bc, req.Provider, req.BYOK)
	// Gateway mode carries max_tokens through ExtraParams (buildChatRequest) ;
	// Bifrost only merges ExtraParams into the outgoing body when passthrough
	// is enabled. Direct mode promotes its knobs to native fields, so it never
	// needs this.
	if !req.BYOK {
		bc.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
	}
	if req.CorrelationID != "" {
		bc.SetTraceAttribute("correlation_id", req.CorrelationID)
	}
	// Per-request caller identity attributed to the gateway/provider : every
	// LLM call is traceable to a session, a user, and a specific agent
	// instance (entry agent id or a distinct sub-agent RunID). Same channel
	// as correlation_id.
	if req.SessionID != "" {
		bc.SetTraceAttribute("session_id", req.SessionID)
	}
	if req.UserID != "" {
		bc.SetTraceAttribute("user_id", req.UserID)
	}
	if req.AgentID != "" {
		bc.SetTraceAttribute("agent_id", req.AgentID)
	}
	// GATEWAY attribution : the gateway reads X-Digitorn-* headers
	// (chatAttribution) to stamp app_id / external_sid / agent_id / run_id
	// on every gateway_usage_events row — the basis for per-app billing,
	// quotas and audit. Gateway-only: never leak identity headers to
	// direct providers (BYOK). Disjoint from applyProviderProtocol's
	// Copilot headers (BYOK-only), so the single ExtraHeaders slot is safe.
	if !req.BYOK {
		setAttributionHeaders(bc, req.AppID, req.SessionID, req.AgentID, req.CorrelationID)
	}
	return bc, cancel, route
}

// setAttributionHeaders installs the X-Digitorn-* identity headers Bifrost
// forwards verbatim on the outgoing HTTP request (BifrostContextKeyExtraHeaders).
// Only non-empty dimensions are sent.
func setAttributionHeaders(bc *schemas.BifrostContext, appID, sessionID, agentID, runID string) {
	hdrs := make(map[string][]string, 4)
	if appID != "" {
		hdrs["X-Digitorn-App-Id"] = []string{appID}
	}
	if sessionID != "" {
		hdrs["X-Digitorn-Session-Id"] = []string{sessionID}
	}
	if agentID != "" {
		hdrs["X-Digitorn-Agent-Id"] = []string{agentID}
	}
	if runID != "" {
		hdrs["X-Digitorn-Run-Id"] = []string{runID}
	}
	if len(hdrs) == 0 {
		return
	}
	bc.SetValue(schemas.BifrostContextKeyExtraHeaders, hdrs)
}

// missingReasoningPlaceholder is sent as reasoning_content for an assistant
// message that carries tool_calls but for which no thinking-mode trace was
// captured (an interrupted or trivial tool call). Reasoning providers (DeepSeek
// thinking mode) require a non-empty reasoning_content on such messages or they
// reject the next request; this keeps the field present and non-empty without
// fabricating a real rationale.
const missingReasoningPlaceholder = "(no reasoning trace recorded for this step)"

func (s *Service) buildChatRequest(req *llm.ChatRequest) *schemas.BifrostChatRequest {
	// Apply Anthropic-style prompt-cache breakpoints on the stable
	// prefix BEFORE wire-format translation. Provider-agnostic: the
	// gateway-go strips cache_control for providers that don't accept it
	// (OpenAI auto-caches, DeepSeek strips, Mistral no-op). See
	// cache_hints.go for the strategy.
	markStablePrefixCacheable(req)

	out := &schemas.BifrostChatRequest{
		Provider: ResolveProvider(req),
		Model:    req.Model,
		Input:    make([]schemas.ChatMessage, 0, len(req.Messages)),
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		bm := schemas.ChatMessage{
			Role: schemas.ChatMessageRole(m.Role),
		}
		// Always emit a `content` field, even empty. An assistant message that
		// only carries tool_calls has no text — most providers tolerate the
		// field being absent, but DeepSeek's strict deserializer rejects the
		// whole request ("missing field `content`"). An empty string is
		// accepted by every OpenAI-compatible provider.
		//
		// Cache control path : if THIS message carries a cache hint OR
		// any of its parts does, we have to switch from the cheap string
		// `content` to the richer block-array form — Anthropic only
		// reads cache_control off content BLOCKS, not the bare string.
		if needsContentBlocks(m) {
			bm.Content = &schemas.ChatMessageContent{ContentBlocks: buildContentBlocks(m)}
		} else {
			c := m.Content
			bm.Content = &schemas.ChatMessageContent{ContentStr: &c}
		}
		if m.Name != "" {
			name := m.Name
			bm.Name = &name
		}
		// Tool result message : role=tool, ToolCallID identifies which
		// assistant tool_call this output answers (OpenAI shape).
		if m.Role == "tool" && m.ToolCallID != "" {
			id := m.ToolCallID
			bm.ChatToolMessage = &schemas.ChatToolMessage{ToolCallID: &id}
		}
		// Prior assistant turn : forward its tool_calls (so the provider can
		// stitch the multi-round conversation) AND its reasoning_content.
		// Reasoning models (DeepSeek thinking mode, xAI) REQUIRE the prior
		// assistant reasoning to be passed back or they reject the request ;
		// bifrost serialises Reasoning as `reasoning_content` for those
		// providers, so this is provider-agnostic — nothing DeepSeek-specific.
		if m.Role == "assistant" && (len(m.ToolCalls) > 0 || m.ReasoningContent != "") {
			am := &schemas.ChatAssistantMessage{}
			if len(m.ToolCalls) > 0 {
				am.ToolCalls = buildAssistantToolCalls(m.ToolCalls)
			}
			switch {
			case m.ReasoningContent != "":
				rc := m.ReasoningContent
				am.Reasoning = &rc
			case len(m.ToolCalls) > 0:
				// DeepSeek thinking mode REJECTS the next request unless every
				// assistant message that carries tool_calls also carries a NON-EMPTY
				// reasoning_content ("...must be passed back to the API") — even when
				// no trace was captured for this turn (an interrupted/quick tool
				// call). An empty string isn't enough (the provider — or a gateway
				// in front of it — treats it as absent), so emit a minimal
				// placeholder. A plain assistant message with no tool_calls and no
				// reasoning keeps the field absent, so a strict provider never sees
				// an empty key.
				rc := missingReasoningPlaceholder
				am.Reasoning = &rc
			}
			bm.ChatAssistantMessage = am
		}
		out.Input = append(out.Input, bm)
	}

	// Params bundles tools + sampling knobs. Build it lazily so non-tool,
	// no-knob requests don't pay the allocation.
	var params *schemas.ChatParameters
	if tools := buildBifrostTools(req.Tools); len(tools) > 0 {
		params = &schemas.ChatParameters{
			Tools:      tools,
			ToolChoice: toolChoiceAuto(),
		}
	}
	if req.Temperature != nil || req.MaxTokens != nil || req.TopP != nil {
		if params == nil {
			params = &schemas.ChatParameters{}
		}
		params.Temperature = req.Temperature
		params.TopP = req.TopP
		if req.MaxTokens != nil {
			if req.BYOK {
				// Direct provider via Bifrost : the modern OpenAI field is
				// correct ; Bifrost translates it per provider (Anthropic
				// max_tokens, o-series max_completion_tokens, …).
				params.MaxCompletionTokens = req.MaxTokens
			} else {
				// Gateway mode : the digitorn LiteLLM gateway validates with a
				// strict allow-list and rejects max_completion_tokens ("Extra
				// inputs are not permitted"). It only accepts the legacy
				// max_tokens, carried through ExtraParams and merged into the
				// body by the passthrough flag set in buildContext.
				if params.ExtraParams == nil {
					params.ExtraParams = map[string]interface{}{}
				}
				params.ExtraParams["max_tokens"] = *req.MaxTokens
			}
		}
	}
	if req.ReasoningEffort != "" {
		if params == nil {
			params = &schemas.ChatParameters{}
		}
		if params.ExtraParams == nil {
			params.ExtraParams = map[string]interface{}{}
		}
		params.ExtraParams["reasoning_effort"] = req.ReasoningEffort
	}
	if params != nil {
		out.Params = params
	}
	// When streaming, request usage in the final chunk so the daemon always
	// gets token counts. Without this, many providers omit usage from
	// streaming responses, leaving the daemon with zero counts.
	if req.Stream {
		if out.Params == nil {
			out.Params = &schemas.ChatParameters{}
		}
		if out.Params.ExtraParams == nil {
			out.Params.ExtraParams = map[string]interface{}{}
		}
		out.Params.ExtraParams["stream_options"] = map[string]interface{}{
			"include_usage": true,
		}
	}
	// Note : Stream is not a field on BifrostChatRequest — instead Bifrost
	// has ChatCompletionStreamRequest() that returns a chan. We dispatch
	// on req.Stream at the Service level, not the request struct.
	return out
}

func mapChatResponse(r *schemas.BifrostChatResponse) *llm.ChatResponse {
	if r == nil {
		return &llm.ChatResponse{}
	}
	out := &llm.ChatResponse{Model: r.Model}
	if len(r.Choices) > 0 {
		ch := r.Choices[0]
		// ChatNonStreamResponseChoice is an embedded pointer ; non-nil
		// for non-streaming responses.
		if ch.ChatNonStreamResponseChoice != nil &&
			ch.ChatNonStreamResponseChoice.Message != nil {
			msg := ch.ChatNonStreamResponseChoice.Message
			if msg.Content != nil && msg.Content.ContentStr != nil {
				out.Content = *msg.Content.ContentStr
			}
			if calls := extractAssistantToolCalls(msg.ChatAssistantMessage); len(calls) > 0 {
				out.ToolCalls = calls
			}
			if msg.ChatAssistantMessage != nil && msg.ChatAssistantMessage.Reasoning != nil {
				out.ReasoningContent = *msg.ChatAssistantMessage.Reasoning
			}
			// Natively-generated images arrive in the assistant message's
			// content blocks (data-URI or remote URL) for providers that
			// surface them through the chat-completions shape.
			if msg.Content != nil {
				for i := range msg.Content.ContentBlocks {
					if mp, ok := imageBlockToMedia(&msg.Content.ContentBlocks[i]); ok {
						out.OutputMedia = append(out.OutputMedia, mp)
					}
				}
			}
		}
		if ch.FinishReason != nil {
			out.FinishReason = *ch.FinishReason
		}
	}
	// Generated videos are surfaced at the response level by bifrost.
	for i := range r.Videos {
		v := r.Videos[i]
		if v.URL == "" {
			continue
		}
		mp := llm.ContentPart{Type: llm.ContentTypeVideo, URL: v.URL}
		if v.ThumbnailURL != nil {
			mp.Thumbnail = *v.ThumbnailURL
		}
		out.OutputMedia = append(out.OutputMedia, mp)
	}
	if r.Usage != nil {
		out.Usage = llm.Usage{
			PromptTokens:     r.Usage.PromptTokens,
			CompletionTokens: r.Usage.CompletionTokens,
			TotalTokens:      r.Usage.TotalTokens,
		}
		// Surface prompt-cache hits/writes so the agent loop can SEE whether the
		// stable-prefix cache is working (and prove the TTFT win). Without this
		// every turn looked like a 0% cache hit even when the provider cached.
		if d := r.Usage.PromptTokensDetails; d != nil {
			out.Usage.CacheReadTokens = d.CachedReadTokens
			out.Usage.CacheWriteTokens = d.CachedWriteTokens
		}
		// Provider-EXACT reasoning tokens (a subset of CompletionTokens). bifrost
		// normalises every provider into completion_tokens_details.reasoning_tokens,
		// so this is generic — one struct read, no hot-path cost.
		if d := r.Usage.CompletionTokensDetails; d != nil {
			out.Usage.ReasoningTokens = d.ReasoningTokens
		}
	}
	return out
}

// imageBlockToMedia converts a bifrost assistant content block into an output
// media part when it carries a generated image — a ``data:<mime>;base64,…``
// URI decodes to inline bytes, a remote ``http(s)`` URL is kept as a URL.
// Returns ok=false for non-image / empty blocks.
func imageBlockToMedia(b *schemas.ChatContentBlock) (llm.ContentPart, bool) {
	if b == nil || b.Type != schemas.ChatContentBlockTypeImage || b.ImageURLStruct == nil {
		return llm.ContentPart{}, false
	}
	url := b.ImageURLStruct.URL
	if url == "" {
		return llm.ContentPart{}, false
	}
	if strings.HasPrefix(url, "data:") {
		// data:<mime>;base64,<payload>
		comma := strings.IndexByte(url, ',')
		if comma < 0 {
			return llm.ContentPart{}, false
		}
		meta, payload := url[len("data:"):comma], url[comma+1:]
		mime := meta
		if semi := strings.IndexByte(meta, ';'); semi >= 0 {
			mime = meta[:semi]
		}
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil || len(data) == 0 {
			return llm.ContentPart{}, false
		}
		if mime == "" {
			mime = "image/png"
		}
		return llm.ContentPart{Type: llm.ContentTypeImage, Mime: mime, Data: data}, true
	}
	return llm.ContentPart{Type: llm.ContentTypeImage, URL: url}, true
}

func mapChatChunk(c *schemas.BifrostStreamChunk) *llm.ChatChunk {
	if c == nil {
		return nil
	}
	if c.BifrostError != nil {
		return &llm.ChatChunk{Error: errMsg(c.BifrostError)}
	}
	cr := c.BifrostChatResponse
	if cr == nil {
		return nil
	}
	out := &llm.ChatChunk{}
	if len(cr.Choices) > 0 {
		ch := cr.Choices[0]
		// For streaming, ChatStreamResponseChoice is the embedded ptr.
		// Tool-call fragments are deliberately NOT mapped here : they
		// arrive split across chunks and must be merged by index by the
		// ChatStream accumulator before they're usable. mapChatChunk
		// only carries the text delta / finish / usage.
		if ch.ChatStreamResponseChoice != nil {
			delta := ch.ChatStreamResponseChoice.Delta
			if delta != nil && delta.Content != nil {
				out.Delta = *delta.Content
			}
			if delta != nil && delta.Reasoning != nil {
				out.ReasoningDelta = *delta.Reasoning
			}
		}
		if ch.FinishReason != nil {
			out.FinishReason = *ch.FinishReason
		}
	}
	// Surface the streaming tool-call fragments (name + arg-size) so the client
	// can render the tool live. The merged, decoded call still flows separately
	// via the accumulator's final flush — these are display-only.
	out.ToolCallDeltas = toolCallDeltaInfos(c)
	if cr.Usage != nil {
		out.Usage = &llm.Usage{
			PromptTokens:     cr.Usage.PromptTokens,
			CompletionTokens: cr.Usage.CompletionTokens,
			TotalTokens:      cr.Usage.TotalTokens,
		}
		if d := cr.Usage.PromptTokensDetails; d != nil {
			out.Usage.CacheReadTokens = d.CachedReadTokens
			out.Usage.CacheWriteTokens = d.CachedWriteTokens
		}
		if d := cr.Usage.CompletionTokensDetails; d != nil {
			out.Usage.ReasoningTokens = d.ReasoningTokens
		}
	}
	return out
}

func errMsg(b *schemas.BifrostError) string {
	if b == nil {
		return ""
	}
	if b.Error != nil && b.Error.Message != "" {
		return b.Error.Message
	}
	if b.Type != nil {
		return *b.Type
	}
	return "unknown bifrost error"
}

// errStatusCode extracts the HTTP status returned by the upstream provider,
// or 0 when Bifrost surfaced an error without one (network drop, parse
// failure, etc.). 0 maps to codes.Unknown downstream.
func errStatusCode(b *schemas.BifrostError) int {
	if b == nil || b.StatusCode == nil {
		return 0
	}
	return *b.StatusCode
}

// httpStatusToGRPCCode maps an upstream HTTP status to the canonical
// gRPC code the dispatcher should surface. Picked to keep
// `client.go::isRetryable` honest:
//
//   - 400, 422            → InvalidArgument (real bug, don't retry)
//   - 401                 → Unauthenticated  (caller fixes creds, don't retry)
//   - 403                 → PermissionDenied (caller fixes scopes, don't retry)
//   - 402                 → FailedPrecondition (billing — surfaces clearly to ops)
//   - 404                 → NotFound (model/provider misconfigured)
//   - 408                 → DeadlineExceeded (retry)
//   - 409                 → Aborted (retry on concurrent-write conflicts)
//   - 429                 → ResourceExhausted (retry with backoff — explicit upstream rate-limit)
//   - 499                 → Canceled (client gave up)
//   - 500, 502, 503, 504  → Unavailable (retry)
//   - 501                 → Unimplemented (don't retry, real config issue)
//   - 0 / unknown         → Unknown (preserve pre-Phase-1 behaviour)
//
// New retry candidates (ResourceExhausted, Unavailable above 500-class)
// are also added to `client.go::isRetryable` so the upgrade flows end
// to end.
func httpStatusToGRPCCode(status int) codes.Code {
	switch status {
	case 400, 422:
		return codes.InvalidArgument
	case 401:
		return codes.Unauthenticated
	case 402:
		return codes.FailedPrecondition
	case 403:
		return codes.PermissionDenied
	case 404:
		return codes.NotFound
	case 408:
		return codes.DeadlineExceeded
	case 409:
		return codes.Aborted
	case 429:
		return codes.ResourceExhausted
	case 499:
		return codes.Canceled
	case 500, 502, 503, 504:
		return codes.Unavailable
	case 501:
		return codes.Unimplemented
	}
	if status >= 500 && status < 600 {
		return codes.Unavailable
	}
	return codes.Unknown
}

// errCallContext carries the per-request identity we want to attach to
// every error log + every gRPC error detail. Built once at the start of
// each handler and re-used.
type errCallContext struct {
	Provider      string
	Model         string
	BYOK          bool
	CorrelationID string
	SessionID     string
	UserID        string
	AgentID       string
	NMessages     int
	NTools        int
}

// errCtxForChat extracts the call context from a chat request — used in
// `Chat`, `ChatStream`, and the in-stream error path.
func errCtxForChat(req *llm.ChatRequest) errCallContext {
	return errCallContext{
		Provider:      req.Provider,
		Model:         req.Model,
		BYOK:          req.BYOK,
		CorrelationID: req.CorrelationID,
		SessionID:     req.SessionID,
		UserID:        req.UserID,
		AgentID:       req.AgentID,
		NMessages:     len(req.Messages),
		NTools:        len(req.Tools),
	}
}

// errCtxForEmbed builds the same shape from an embed request — embed
// requests don't carry conversation state so NMessages / NTools stay 0.
// llm.EmbedRequest does not currently carry CorrelationID / SessionID /
// UserID / AgentID; if/when those are added to the embed surface, copy
// them here. Until then the embed error logs are slightly thinner than
// the chat ones, but the provider/model/status_code triad is the bulk
// of the diagnostic value anyway.
func errCtxForEmbed(req *llm.EmbedRequest) errCallContext {
	return errCallContext{
		Provider: req.Provider,
		Model:    req.Model,
		BYOK:     req.BYOK,
	}
}

// translateError converts a Bifrost error into a gRPC error that
// preserves the upstream status code, provider, model, and call
// identity. Critical Phase-1 fix: before this, every error was
// flattened to gRPC `codes.Unknown` via plain `fmt.Errorf`, which
// caused `client.go::isRetryable` to never trigger and made every
// 400 indistinguishable from a 5xx in monitoring.
//
// The returned error is a `*status.Status.Err()` — gRPC handlers
// return it verbatim and the gRPC framework propagates the code +
// details to the calling client. Callers using `status.FromError`
// can read both the message and the `errdetails.ErrorInfo` to
// surface the provider name + status code in dashboards / retry
// decisions.
func translateError(berr *schemas.BifrostError, ec errCallContext) error {
	statusCode := errStatusCode(berr)
	grpcCode := httpStatusToGRPCCode(statusCode)
	msg := errMsg(berr)
	if msg == "" {
		msg = "unknown bifrost error"
	}

	// Compose a gRPC status with the upstream context embedded as a
	// machine-readable detail proto. Dashboards / alerts can match on
	// `ErrorInfo.Reason` to e.g. group all "anthropic_400" failures.
	st := status.New(grpcCode, fmt.Sprintf("bifrost: %s", msg))
	info := &errdetails.ErrorInfo{
		Reason: fmt.Sprintf("%s_%d", ec.Provider, statusCode),
		Domain: "bifrost",
		Metadata: map[string]string{
			"provider":       ec.Provider,
			"model":          ec.Model,
			"status_code":    fmt.Sprintf("%d", statusCode),
			"correlation_id": ec.CorrelationID,
			"session_id":     ec.SessionID,
			"user_id":        ec.UserID,
			"agent_id":       ec.AgentID,
		},
	}
	if dst, derr := st.WithDetails(info); derr == nil {
		return dst.Err()
	}
	// WithDetails failed (very rare — proto marshal error). Still
	// return the typed status so the gRPC code propagates correctly,
	// just without the structured detail.
	return st.Err()
}

// logBifrostError writes a single structured ERROR log capturing every
// piece of context an operator needs to diagnose. Called by every
// Bifrost error path (Chat, ChatStream initial, ChatStream mid-stream,
// Embed). Cheap (~5 µs); the value is night-and-day visibility.
//
// IMPORTANT: this never mutates state. Pure observability. Disabling
// the logger or raising the level filters the call cleanly.
func (s *Service) logBifrostError(berr *schemas.BifrostError, ec errCallContext, path string) {
	logger := s.cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	statusCode := errStatusCode(berr)
	logger.Error("bifrost.error",
		slog.String("path", path),
		slog.String("provider", ec.Provider),
		slog.String("model", ec.Model),
		slog.Bool("byok", ec.BYOK),
		slog.Int("status_code", statusCode),
		slog.String("grpc_code", httpStatusToGRPCCode(statusCode).String()),
		slog.String("correlation_id", ec.CorrelationID),
		slog.String("session_id", ec.SessionID),
		slog.String("user_id", ec.UserID),
		slog.String("agent_id", ec.AgentID),
		slog.Int("n_messages", ec.NMessages),
		slog.Int("n_tools", ec.NTools),
		slog.String("err", errMsg(berr)),
	)
}

func estimateTokens(msgs []llm.ChatMessage) int {
	// Very rough heuristic ; V2 will plug provider-specific tokenizers.
	total := 0
	for _, m := range msgs {
		total += len(m.Content)/4 + 4
	}
	return total
}
