// Package bifrost wraps github.com/maximhq/bifrost/core to expose the
// llm.Service interface to the rest of digitorn. The wrapper is THIN :
// it only handles routing (gateway vs direct), credential injection,
// and translation between digitorn's llm.* types and Bifrost's schemas.*
// types. No business logic about rate-limits, quotas or cost lives here
// (the digitorn external gateway owns that).
package bifrost

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	schemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/digitornai/digitorn/internal/llm"
)

// routeInfoPool recycles routeInfo structs across requests. The struct
// is small but allocated on every Chat/Embed; pooling drops 1 heap alloc
// per call. ctx.Value forces the pooled value to escape via interface{}
// — pool still wins on the alloc-rate side because Get returns a hot
// pointer instead of triggering mallocgc.
var routeInfoPool = sync.Pool{
	New: func() any { return new(routeInfo) },
}

func acquireRouteInfo(byok bool, apiKey, userJWT, baseURL string) *routeInfo {
	r := routeInfoPool.Get().(*routeInfo)
	r.BYOK = byok
	r.APIKey = apiKey
	r.UserJWT = userJWT
	r.BaseURL = baseURL
	return r
}

// releaseRouteInfo returns r to the pool. Caller must ensure no other
// goroutine still references it (Bifrost reads it during the request,
// so release happens only after the call returns).
func releaseRouteInfo(r *routeInfo) {
	if r == nil {
		return
	}
	*r = routeInfo{}
	routeInfoPool.Put(r)
}

// Config is what the worker binary passes at startup.
type Config struct {
	// GatewayURL is the digitorn LLM gateway base URL. Used when
	// the per-request route says BYOK=false.
	GatewayURL string

	// Concurrency / BufferSize defaults applied to every provider unless
	// overridden by PerProviderConcurrency / PerProviderBufferSize.
	Concurrency int
	BufferSize  int

	// Circuit breaker tuning. Defaults — applied when each field is 0 —
	// are deliberately responsive (threshold 3, window 30s, openFor 5s)
	// to favour quick recovery on transient network blips. Bump openFor
	// to 30s+ in production to avoid hammering a struggling provider.
	CBThreshold int           // failures within Window before tripping OPEN (default 3)
	CBWindow    time.Duration // rolling failure window (default 30s)
	CBOpenFor   time.Duration // cooldown before HALF-OPEN probe (default 5s)

	// PerProviderConcurrency maps a normalised provider name (lower-case
	// llm.ChatRequest.Provider) to a custom Bifrost concurrency cap.
	// Used when a provider sustains very different RPS than the
	// global default — e.g. Anthropic 1000 vs DeepSeek 50.
	// Empty / missing → fall back to global Concurrency.
	PerProviderConcurrency map[string]int

	// PerProviderBufferSize maps a provider name to a custom Bifrost
	// in-flight queue size. Bigger = absorbs more burst, more memory.
	// Empty / missing → fall back to global BufferSize.
	PerProviderBufferSize map[string]int

	// AuditEnabled toggles the audit log plugin (1 line per request).
	// Default off — turn on for local debugging only.
	AuditEnabled bool

	// Logger is used by the audit plugin. nil → slog.Default().
	Logger *slog.Logger
}

// ctxKey is the type used to stash per-request routing info in the
// Bifrost context so the Account can pick it up. We use a SINGLE key
// pointing at *routeInfo to keep the ctx.Value lookup chain at depth 1
// (one map probe instead of three).
type ctxKey int

const ctxKeyRoute ctxKey = iota

// routeInfo carries the daemon's pre-resolved routing decision. It is
// allocated once per request and stashed in the Bifrost context under
// ctxKeyRoute. All fields are read-only after the service hands the
// request off to Bifrost.
type routeInfo struct {
	BYOK    bool   // true → DIRECT, false → GATEWAY
	APIKey  string // populated iff BYOK
	UserJWT string // populated iff !BYOK
	BaseURL string // optional in both modes
}

// account implements bifrost/schemas.Account. It looks at the per-request
// context (populated by the service before each call) to decide which
// credentials to surface to Bifrost. The decision itself is O(1) — one
// ctx.Value lookup + one bool branch.
type account struct {
	cfg Config
}

func newAccount(cfg Config) *account {
	return &account{cfg: cfg}
}

func (a *account) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	// All providers Bifrost ships. We don't gate at this layer ; the
	// gateway / per-app config decides what's actually usable.
	return []schemas.ModelProvider{
		schemas.OpenAI, schemas.Anthropic, schemas.Azure, schemas.Bedrock,
		schemas.Cohere, schemas.Mistral, schemas.Gemini, schemas.Vertex,
		schemas.Groq, schemas.Fireworks, schemas.Perplexity, schemas.Cerebras,
		schemas.Ollama, schemas.VLLM, schemas.OpenRouter, schemas.HuggingFace,
		schemas.XAI, schemas.SGL, schemas.Nebius, schemas.Parasail,
		schemas.Replicate,
	}, nil
}

// GetKeysForProvider injects the credential resolved at request time.
// The routing decision was already taken by the daemon (BYOK bool) ; we
// just branch on it. O(1) : 1 ctx.Value lookup, 1 bool branch, 0 allocs
// on the happy path (the Key is heap-escaped by the return but that's
// unavoidable given Bifrost's API).
func (a *account) GetKeysForProvider(ctx context.Context, _ schemas.ModelProvider) ([]schemas.Key, error) {
	r, _ := ctx.Value(ctxKeyRoute).(*routeInfo)
	if r == nil {
		return nil, errors.New("bifrost: no route info in context")
	}
	if r.BYOK {
		// Route exactly as configured: send the key the user provided, nothing
		// more. A local / self-hosted OpenAI-compatible endpoint (LM Studio,
		// Ollama…) has a base_url and NO key → we send no key. Only a missing key
		// AND no base_url is a real error (a cloud provider needs one).
		if r.APIKey == "" && r.BaseURL == "" {
			return nil, errors.New("bifrost: BYOK requested but neither APIKey nor BaseURL is set")
		}
		key := schemas.Key{
			ID:     "direct",
			Value:  schemas.EnvVar{Val: r.APIKey, FromEnv: false},
			Models: schemas.WhiteList{"*"},
			Weight: 1.0,
		}
		// BYOK + custom BaseURL: attach it as VLLMKeyConfig so the VLLM
		// provider uses this URL instead of the provider-level default. The VLLM
		// provider appends "/v1/chat/completions" itself, so the base must NOT
		// already end in /v1 (a base stored as ".../v1" would become
		// ".../v1/v1/..." → 404 / non-SSE). Strip a trailing /v1 so both the
		// with- and without-/v1 forms the user might store work.
		if r.BaseURL != "" {
			base := strings.TrimSuffix(strings.TrimRight(r.BaseURL, "/"), "/v1")
			key.VLLMKeyConfig = &schemas.VLLMKeyConfig{
				URL: schemas.EnvVar{Val: base, FromEnv: false},
			}
		}
		return []schemas.Key{key}, nil
	}
	if r.UserJWT == "" {
		return nil, errors.New("bifrost: gateway mode requires UserJWT")
	}
	return []schemas.Key{{
		ID:     "gateway",
		Value:  schemas.EnvVar{Val: r.UserJWT, FromEnv: false},
		Models: schemas.WhiteList{"*"},
		Weight: 1.0,
	}}, nil
}

func (a *account) GetConfigForProvider(p schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	// Per-provider override takes precedence over the global default,
	// itself floored to keep Bifrost happy (Concurrency≥8, BufferSize≥256).
	concurrency := a.cfg.Concurrency
	if v, ok := a.cfg.PerProviderConcurrency[strings.ToLower(string(p))]; ok && v > 0 {
		concurrency = v
	}
	bufferSize := a.cfg.BufferSize
	if v, ok := a.cfg.PerProviderBufferSize[strings.ToLower(string(p))]; ok && v > 0 {
		bufferSize = v
	}
	cfg := &schemas.ProviderConfig{
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{
			Concurrency: maxInt(concurrency, 8),
			BufferSize:  maxInt(bufferSize, 256),
		},
	}
	// Gateway path : ResolveProvider maps every gateway request to
	// schemas.OpenAI (the gateway speaks OpenAI-compatible via LiteLLM).
	// Bifrost defaults to api.openai.com if NetworkConfig.BaseURL is
	// empty, which is wrong for our setup -- we'd dial OpenAI directly
	// with the user's digitorn JWT and get "issuer invalid". Pin the
	// OpenAI provider to the configured gateway URL.
	//
	// Bifrost's OpenAI provider appends `/v1/chat/completions` to the
	// BaseURL, so we strip a trailing `/v1` (the canonical Python ref
	// convention is to write `http://host:port/v1`) to avoid double-v1
	// 404s.
	if p == schemas.OpenAI && a.cfg.GatewayURL != "" {
		cfg.NetworkConfig.BaseURL = strings.TrimSuffix(
			strings.TrimRight(a.cfg.GatewayURL, "/"), "/v1")
	}
	return cfg, nil
}

// ResolveProvider maps digitorn's free-form Provider string to a
// Bifrost ModelProvider. The decision is O(1) : one bool check then
// at most one switch lookup. For gateway routing we always use the
// OpenAI-compatible adapter (the gateway speaks OpenAI via LiteLLM,
// regardless of the underlying provider).
// applyProviderProtocol installs provider-specific wire details that the
// generic OpenAI-compatible path can't infer. GitHub Copilot: the resolver
// already swapped the stored GitHub token for a Copilot session token +
// https://api.githubcopilot.com, but the API serves /chat/completions (no /v1
// prefix) and requires editor identification headers — both injected here via
// Bifrost's per-request context hooks. Same role as ResolveProvider's switch:
// a protocol adapter keyed by the provider slug, not configuration.
func applyProviderProtocol(bc *schemas.BifrostContext, provider string, byok bool) {
	if !byok || !strings.EqualFold(provider, "github_copilot") {
		return
	}
	bc.SetValue(schemas.BifrostContextKeyURLPath, "/chat/completions")
	bc.SetValue(schemas.BifrostContextKeyExtraHeaders, map[string][]string{
		"Copilot-Integration-Id": {"vscode-chat"},
		"Editor-Version":         {"vscode/1.96.0"},
		"Editor-Plugin-Version":  {"copilot-chat/0.27.0"},
		"User-Agent":             {"GithubCopilot/1.270.0"},
	})
}

func ResolveProvider(req *llm.ChatRequest) schemas.ModelProvider {
	if !req.BYOK {
		return schemas.OpenAI
	}
	// BYOK + custom BaseURL: route through VLLM provider which supports
	// per-key URL overrides (VLLMKeyConfig.URL). The VLLM provider speaks
	// OpenAI-compatible API, so it works with any OpenAI-compatible endpoint.
	// This covers any provider with a custom base_url (e.g. opencode, ollama,
	// vLLM, or any OpenAI-compatible endpoint) — not just "openai".
	if req.BaseURL != "" {
		return schemas.VLLM
	}
	switch strings.ToLower(req.Provider) {
	case "anthropic":
		return schemas.Anthropic
	case "openai":
		return schemas.OpenAI
	case "azure":
		return schemas.Azure
	case "bedrock":
		return schemas.Bedrock
	case "cohere":
		return schemas.Cohere
	case "mistral":
		return schemas.Mistral
	case "gemini", "google-gemini":
		return schemas.Gemini
	case "vertex":
		return schemas.Vertex
	case "groq":
		return schemas.Groq
	case "fireworks":
		return schemas.Fireworks
	case "perplexity":
		return schemas.Perplexity
	case "cerebras":
		return schemas.Cerebras
	case "ollama":
		return schemas.Ollama
	case "vllm":
		return schemas.VLLM
	case "openrouter":
		return schemas.OpenRouter
	case "huggingface":
		return schemas.HuggingFace
	case "xai", "grok":
		return schemas.XAI
	case "sgl":
		return schemas.SGL
	case "nebius":
		return schemas.Nebius
	case "parasail":
		return schemas.Parasail
	case "replicate":
		return schemas.Replicate
	}
	// Unknown / future providers route through the OpenAI-compatible path.
	return schemas.OpenAI
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
