// Package llm defines the public LLM API that the digitorn runtime
// consumes. The runtime sees ONLY these types ; gRPC, Bifrost, worker
// subprocesses are hidden by the Client. Changing the wire format
// (JSON ↔ protobuf, transport, etc.) MUST NOT change this file.
package llm

import "time"

// ChatRequest is what the runtime sends. Routing (gateway vs. direct
// provider) is decided by the BYOK flag — an explicit per-app policy
// resolved by the daemon BEFORE the call. The worker performs ZERO
// content inspection ; it reads BYOK in O(1) and dispatches.
type ChatRequest struct {
	// BYOK ("Bring Your Own Key") selects the routing mode.
	//   false (default) → GATEWAY : Bifrost calls the digitorn LLM gateway
	//                     using UserJWT as bearer credential. The gateway
	//                     owns quotas, cost, rate-limits.
	//   true            → DIRECT  : Bifrost calls the provider's native API
	//                     using APIKey (+ optional BaseURL) supplied by
	//                     the daemon from the app's BYOK credential store.
	//
	// The daemon resolves BYOK from the app policy (Python:
	// user_app_byok.enabled per (user_id, app_id)) BEFORE invoking the
	// client. The worker never queries the DB itself.
	BYOK bool `json:"byok,omitempty"`

	// Provider is the canonical name : "anthropic", "openai", "deepseek", …
	// Used as routing key in DIRECT mode and as the model namespace in
	// GATEWAY mode (the gateway dispatches internally via LiteLLM).
	Provider string `json:"provider"`

	// Model is the model identifier. For gateway routing it's the
	// LiteLLM-style "provider/model" (e.g. "anthropic/claude-sonnet-4.5").
	// For direct routing, it's the provider-native name.
	Model string `json:"model"`

	// APIKey is the provider credential used in DIRECT mode (BYOK=true).
	// Ignored when BYOK=false.
	APIKey string `json:"api_key,omitempty"`

	// UserJWT is the digitorn user JWT used in GATEWAY mode (BYOK=false)
	// as the bearer credential. Ignored when BYOK=true.
	UserJWT string `json:"user_jwt,omitempty"`

	// BaseURL is the custom base URL (e.g. for ollama, vllm self-hosted,
	// or a non-default gateway). "" means the worker picks the default
	// for the routing mode.
	BaseURL string `json:"base_url,omitempty"`

	// Messages is the conversation. Roles : user, assistant, system, tool.
	Messages []ChatMessage `json:"messages"`

	// Tools is the optional tool catalog passed to the LLM.
	Tools []ToolSpec `json:"tools,omitempty"`

	// Stream : if true, the worker uses streaming. The Chat method itself
	// is non-streaming ; ChatStream is the streaming variant.
	Stream bool `json:"stream,omitempty"`

	// Temperature, MaxTokens, TopP, etc. — provider-agnostic knobs.
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	// Timeout caps the request duration. 0 = use worker default.
	Timeout time.Duration `json:"timeout,omitempty"`

	// CorrelationID propagates a daemon-side request ID for tracing.
	CorrelationID string `json:"correlation_id,omitempty"`

	// SessionID, UserID, AgentID attribute the request to a session, a
	// user, and a specific agent instance (the entry agent's id, or a
	// distinct sub-agent RunID like "coding#a1b2c3"). The worker forwards
	// them to the gateway/provider as trace attributes + X-Digitorn-*
	// headers so every LLM call is attributable end to end. Empty =
	// anonymous / system call.
	SessionID string `json:"session_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

// ChatMessage is one item in the conversation.
//
// Parts is the multi-part native shape (text + images + audio in one
// message), matching what modern LLM providers expect on the wire.
// Content is kept for back-compat with text-only writers / single-part
// callers ; if both are set, Parts wins.
//
// The runtime fills Parts from the persisted sessionstore.MessagePart
// list, inlining blob bytes (read from the daemon's blob store) into
// ContentPart.Data. The worker stays oblivious to the blob store —
// it only sees inline bytes.
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	Parts      []ContentPart  `json:"parts,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	Name       string         `json:"name,omitempty"`

	// ReasoningContent is the thinking-mode trace from a prior assistant turn,
	// replayed back to the provider. Reasoning models (DeepSeek thinking mode,
	// xAI) require the reasoning_content of earlier assistant messages to be
	// passed back or they reject the request — the worker maps this onto the
	// provider's reasoning_content field, provider-agnostically via bifrost.
	ReasoningContent string `json:"reasoning_content,omitempty"`

	// CacheControl marks this message as a cache breakpoint. Anthropic-family
	// providers cache the prompt prefix up to and including this message.
	// Strip-friendly: gateway-go's normalisation layer removes this field for
	// providers that don't accept it (OpenAI / DeepSeek auto-cache without
	// hints, Mistral / Cohere don't support caching at all). So the daemon
	// can mark messages universally — the wire format dispatch is handled
	// downstream.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// CacheControl is the Anthropic-style cache breakpoint hint. Bifrost
// understands this struct natively and forwards it to providers that
// support prompt caching.
//
// Type "ephemeral" = ~5 min cache TTL (Anthropic default). When set on a
// content block, system message, tool def, or assistant message, the
// provider caches the prompt prefix up to and including that block.
//
// Anthropic accepts up to 4 cache breakpoints per request — strategic
// placement (system + tools + mid-history + recent-history) gives layered
// caches with LRU eviction.
type CacheControl struct {
	Type string `json:"type"` // "ephemeral" — only value currently
}

// ContentPart is one chunk of a multi-part ChatMessage content. The
// discriminator is Type ; exactly one of Text / Data / URL is filled
// depending on Type.
//
// For binary parts (image, audio, file) the daemon-side adapter loads
// the bytes from the blob store and inlines them via Data. The worker
// then base64-encodes (OpenAI) or wraps in `source.data` (Anthropic) at
// serialisation time. Mime is required for binary parts.
//
// URL is the alternative for "the resource is publicly fetchable by the
// provider" — useful for image_url style requests that skip our blob
// store entirely. Mutually exclusive with Data.
type ContentPart struct {
	Type string `json:"type"` // "text" | "image" | "audio" | "video" | "file"

	Text string `json:"text,omitempty"`

	// Inline binary content. Caller pre-loads from storage.
	Mime string `json:"mime,omitempty"`
	Data []byte `json:"data,omitempty"`
	URL  string `json:"url,omitempty"`

	// Display metadata for generated media (assistant output parts).
	Name      string `json:"name,omitempty"`
	Thumbnail string `json:"thumbnail,omitempty"`

	// CacheControl on the block level. Used when ChatMessage.Parts has
	// multiple blocks and only the LAST one carries the breakpoint —
	// Anthropic caches up to and including the marked block, so finer
	// granularity than the message level is sometimes valuable.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// Universal Type constants so producers and consumers agree on strings.
const (
	ContentTypeText  = "text"
	ContentTypeImage = "image"
	ContentTypeAudio = "audio"
	ContentTypeVideo = "video"
	ContentTypeFile  = "file"
)

// ChatToolCall is the LLM's request to invoke a tool.
type ChatToolCall struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"` // "function"
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// ChatToolCallDelta is a streaming fragment of a tool call, surfaced BEFORE the
// call is complete so the client can show the tool name the instant it's
// detected and grow a token counter while the (possibly huge) arguments stream
// in. Keyed by Index ; ID/Name arrive on the first fragment, ArgsChars is the
// byte length of THIS fragment's argument slice (the engine accumulates it into
// a live token estimate). The complete, decoded call still arrives separately.
type ChatToolCallDelta struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	ArgsChars int    `json:"args_chars,omitempty"`
}

// ArgsArrayKey is the sentinel key under which a tool's arguments are preserved
// when a model emits a BARE JSON array instead of an object (common for
// list-shaped tools like run_parallel). The decode layer stashes the array here
// so a liberal tool parser can recover it rather than dropping the call.
const ArgsArrayKey = "__args_array"

// ToolSpec describes one tool the LLM may call.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"` // JSON Schema

	// CacheControl marks the tool definition as a cache breakpoint.
	// Typically only the LAST tool in the list carries this — Anthropic
	// caches the whole tool array up to and including the marked entry.
	CacheControl *CacheControl `json:"cache_control,omitempty"`

	// Canonical is the internal dotted FQN this tool dispatches to when the
	// wire Name is a model-friendly alias rather than the canonical form.
	// MCP virtual tools ship a short Name ("echo") the LLM can call cleanly
	// while resolving to "mcp_<server>.<tool>" internally; the inbound
	// canonicalizer maps the returned tool_call back via this field. Empty
	// when Name already IS the canonical form (every native tool). Never
	// serialised to the provider.
	Canonical string `json:"-"`
}

// ChatResponse is the non-streaming result.
type ChatResponse struct {
	Content      string         `json:"content,omitempty"`
	ToolCalls    []ChatToolCall `json:"tool_calls,omitempty"`
	FinishReason string         `json:"finish_reason,omitempty"`
	Model        string         `json:"model,omitempty"`
	Usage        Usage          `json:"usage"`

	// ReasoningContent is the model's thinking-mode trace for this response,
	// kept separate from Content. Persisted with the assistant message so it
	// round-trips back to providers that require it (DeepSeek thinking mode).
	ReasoningStartedAt int64 `json:"reasoning_started_at,omitempty"`
	ReasoningEndedAt   int64 `json:"reasoning_ended_at,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`

	// OutputMedia carries natively-generated media (image/video) returned as
	// the assistant's answer. Each part is image/video with either inline
	// ``Data`` (decoded data-URI) or a remote ``URL`` (e.g. a generated video).
	OutputMedia []ContentPart `json:"output_media,omitempty"`
}

// ChatChunk is one streaming delta. Successive chunks of the same request
// progressively reveal Content / ToolCalls / Usage.
type ChatChunk struct {
	Delta        string         `json:"delta,omitempty"`
	ToolCalls    []ChatToolCall `json:"tool_calls,omitempty"`
	FinishReason string         `json:"finish_reason,omitempty"`
	Usage        *Usage         `json:"usage,omitempty"`
	Error        string         `json:"error,omitempty"`

	// ReasoningDelta is the incremental thinking-mode trace for this chunk
	// (DeepSeek/o-series). Accumulated separately from Delta into the final
	// response's ReasoningContent.
	ReasoningDelta string `json:"reasoning_delta,omitempty"`

	// ToolCallDeltas surfaces streaming tool-call fragments (name + growing
	// argument size) so the client can render a tool the instant the model
	// starts emitting it, instead of staying blind until the whole call (e.g.
	// a large filesystem.write) finishes accumulating.
	ToolCallDeltas []ChatToolCallDelta `json:"tool_call_deltas,omitempty"`
}

// Usage reports token counts. The gateway is the authoritative source
// for cost ; the worker just forwards what the provider reported.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// ReasoningTokens is the provider's EXACT count of tokens spent on the
	// model's hidden reasoning/thinking — a SUBSET already included in
	// CompletionTokens (OpenAI convention, which bifrost normalises for every
	// provider via completion_tokens_details.reasoning_tokens). Surfaced as a
	// breakdown so clients can show it; never add it on top of CompletionTokens.
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// EmbedRequest / EmbedResponse — future, declared now for shape stability.
type EmbedRequest struct {
	BYOK     bool          `json:"byok,omitempty"`
	Provider string        `json:"provider"`
	Model    string        `json:"model"`
	APIKey   string        `json:"api_key,omitempty"`
	UserJWT  string        `json:"user_jwt,omitempty"`
	BaseURL  string        `json:"base_url,omitempty"`
	Input    []string      `json:"input"`
	Timeout  time.Duration `json:"timeout,omitempty"`
}

type EmbedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
	Model      string      `json:"model"`
	Usage      Usage       `json:"usage"`
}

// ListProvidersResponse enumerates what the worker knows how to dial.
type ListProvidersResponse struct {
	Providers []ProviderInfo `json:"providers"`
}

type ProviderInfo struct {
	Name         string   `json:"name"`
	Modalities   []string `json:"modalities"`    // "chat", "embed", "image", "speech", "ocr"
	GatewayReady bool     `json:"gateway_ready"` // true if the gateway path works for this provider
}
