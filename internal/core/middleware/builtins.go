package middleware

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/digitornai/digitorn/internal/ports"
)

// Retriever is the RAG retrieval seam: query -> ranked context chunks. nil =
// rag_inject is inert (matches the reference daemon, which no-ops without a
// retriever). Production wires this to a knowledge-base backend.
type Retriever func(ctx context.Context, query string) ([]string, error)

// Deps are the runtime dependencies injected into middleware at build time
// (kept out of the YAML config).
type Deps struct {
	Retriever Retriever

	// CustomFactory resolves a `custom` middleware entry into an
	// AppMiddleware (the gRPC/process plugin transport). nil = `custom`
	// middleware is unavailable (skipped with a warning). Wired by the
	// runtime/server layer ; keeps this package free of the worker import.
	CustomFactory func(name string, cfg map[string]any) (ports.AppMiddleware, error)
}

// ---- mask_secrets ---------------------------------------------------------

var defaultSecretPatterns = []string{
	`(?i)(password|passwd|pwd)\s*(?:[:=]|is)\s*\S+`,
	`(?i)(api[_-]?key|apikey)\s*(?:[:=]|is)\s*\S+`,
	`(?i)(secret[_-]?key|secret)\s*(?:[:=]|is)\s*\S+`,
	`(?i)(token|auth[_-]?token|access[_-]?token)\s*(?:[:=]|is)\s*\S+`,
	`(?i)(bearer\s+)\S+`,
	`sk-[a-zA-Z0-9]{20,}`,
	`ghp_[a-zA-Z0-9]{36}`,
	`glpat-[a-zA-Z0-9\-]{20,}`,
}

type maskSecrets struct {
	patterns    []*regexp.Regexp
	replacement string
}

func newMaskSecrets(cfg map[string]any, _ Deps) (ports.AppMiddleware, error) {
	m := &maskSecrets{replacement: cfgStr(cfg, "replacement", "[MASKED]")}
	for _, p := range defaultSecretPatterns {
		if re, err := regexp.Compile(p); err == nil {
			m.patterns = append(m.patterns, re)
		}
	}
	for _, p := range cfgStrSlice(cfg, "patterns") {
		if re, err := regexp.Compile(`(?i)(` + p + `)\s*[:=]\s*\S+`); err == nil {
			m.patterns = append(m.patterns, re)
		}
	}
	return m, nil
}

func (m *maskSecrets) Name() string { return "mask_secrets" }

func (m *maskSecrets) mask(text string) string {
	for _, re := range m.patterns {
		text = re.ReplaceAllString(text, m.replacement)
	}
	return text
}

func (m *maskSecrets) Before(_ context.Context, mctx *ports.MiddlewareContext) (string, bool, error) {
	for i := range mctx.Messages {
		if mctx.Messages[i].Role == "user" {
			mctx.Messages[i].Content = m.mask(mctx.Messages[i].Content)
		}
	}
	return "", false, nil
}

func (m *maskSecrets) After(_ context.Context, _ *ports.MiddlewareContext, response string, _ []ports.LLMToolCall) (string, error) {
	if response == "" {
		return response, nil
	}
	return m.mask(response), nil
}

// ---- prompt_inject --------------------------------------------------------

type promptInject struct {
	system   string
	position string
}

func newPromptInject(cfg map[string]any, _ Deps) (ports.AppMiddleware, error) {
	return &promptInject{
		system:   cfgStr(cfg, "system", ""),
		position: cfgStr(cfg, "position", "append"),
	}, nil
}

func (p *promptInject) Name() string { return "prompt_inject" }

func (p *promptInject) Before(_ context.Context, mctx *ports.MiddlewareContext) (string, bool, error) {
	if p.system == "" {
		return "", false, nil
	}
	if p.position == "prepend" {
		mctx.SystemPrompt = p.system + "\n\n" + mctx.SystemPrompt
	} else {
		mctx.SystemPrompt = mctx.SystemPrompt + "\n\n" + p.system
	}
	return "", false, nil
}

func (p *promptInject) After(_ context.Context, _ *ports.MiddlewareContext, response string, _ []ports.LLMToolCall) (string, error) {
	return response, nil
}

// ---- content_filter -------------------------------------------------------

type contentFilter struct {
	patterns  []*regexp.Regexp
	rejection string
}

func newContentFilter(cfg map[string]any, _ Deps) (ports.AppMiddleware, error) {
	c := &contentFilter{rejection: cfgStr(cfg, "rejection_message", "This request has been blocked by content filter.")}
	for _, p := range cfgStrSlice(cfg, "block_patterns") {
		if re, err := regexp.Compile(`(?i)` + p); err == nil {
			c.patterns = append(c.patterns, re)
		}
	}
	return c, nil
}

func (c *contentFilter) Name() string { return "content_filter" }

func (c *contentFilter) Before(_ context.Context, mctx *ports.MiddlewareContext) (string, bool, error) {
	// Check only the most recent user message (mirrors the reference).
	for i := len(mctx.Messages) - 1; i >= 0; i-- {
		if mctx.Messages[i].Role != "user" {
			continue
		}
		for _, re := range c.patterns {
			if re.MatchString(mctx.Messages[i].Content) {
				return c.rejection, true, nil
			}
		}
		break
	}
	return "", false, nil
}

func (c *contentFilter) After(_ context.Context, _ *ports.MiddlewareContext, response string, _ []ports.LLMToolCall) (string, error) {
	return response, nil
}

// ---- rag_inject -----------------------------------------------------------

type ragInject struct {
	maxChunks int
	maxChars  int
	position  string
	retriever Retriever
}

func newRagInject(cfg map[string]any, deps Deps) (ports.AppMiddleware, error) {
	return &ragInject{
		maxChunks: cfgInt(cfg, "max_chunks", 5),
		maxChars:  cfgInt(cfg, "max_chars", 2000),
		position:  cfgStr(cfg, "position", "append"),
		retriever: deps.Retriever,
	}, nil
}

func (r *ragInject) Name() string { return "rag_inject" }

func (r *ragInject) Before(ctx context.Context, mctx *ports.MiddlewareContext) (string, bool, error) {
	if r.retriever == nil {
		return "", false, nil
	}
	query := ""
	for i := len(mctx.Messages) - 1; i >= 0; i-- {
		if mctx.Messages[i].Role == "user" {
			query = mctx.Messages[i].Content
			break
		}
	}
	if query == "" {
		return "", false, nil
	}
	chunks, err := r.retriever(ctx, query)
	if err != nil || len(chunks) == 0 {
		return "", false, nil // best-effort, like the reference
	}
	if len(chunks) > r.maxChunks {
		chunks = chunks[:r.maxChunks]
	}
	var formatted []string
	total := 0
	for i, chunk := range chunks {
		if total+len(chunk) > r.maxChars {
			chunk = truncateUTF8(chunk, r.maxChars-total)
			formatted = append(formatted, formatChunk(i+1, chunk))
			break
		}
		formatted = append(formatted, formatChunk(i+1, chunk))
		total += len(chunk)
	}
	ragBlock := "\n\n--- Relevant context (retrieved automatically) ---\n" +
		strings.Join(formatted, "\n") + "\n--- End of context ---"
	if r.position == "prepend" {
		mctx.SystemPrompt = ragBlock + "\n\n" + mctx.SystemPrompt
	} else {
		mctx.SystemPrompt = mctx.SystemPrompt + ragBlock
	}
	return "", false, nil
}

func (r *ragInject) After(_ context.Context, _ *ports.MiddlewareContext, response string, _ []ports.LLMToolCall) (string, error) {
	return response, nil
}

func formatChunk(i int, chunk string) string {
	return "[" + strconv.Itoa(i) + "] " + chunk
}

// ---- response_filter ------------------------------------------------------

type responseFilter struct {
	maxLength int
	masker    *maskSecrets
}

func newResponseFilter(cfg map[string]any, _ Deps) (ports.AppMiddleware, error) {
	rf := &responseFilter{maxLength: cfgInt(cfg, "max_length", 0)}
	if cfgBool(cfg, "mask_secrets", false) {
		m, _ := newMaskSecrets(nil, Deps{})
		rf.masker = m.(*maskSecrets)
	}
	return rf, nil
}

func (r *responseFilter) Name() string { return "response_filter" }

func (r *responseFilter) Before(_ context.Context, _ *ports.MiddlewareContext) (string, bool, error) {
	return "", false, nil
}

func (r *responseFilter) After(_ context.Context, _ *ports.MiddlewareContext, response string, _ []ports.LLMToolCall) (string, error) {
	if r.masker != nil {
		response = r.masker.mask(response)
	}
	if r.maxLength > 0 && len(response) > r.maxLength {
		response = truncateUTF8(response, r.maxLength) + "\n\n[Response truncated]"
	}
	return response, nil
}

// truncateUTF8 returns at most max bytes of s WITHOUT splitting a multi-byte
// rune. A raw s[:max] can cut a UTF-8 sequence mid-rune, producing invalid
// bytes that corrupt the prompt / JSON payload downstream.
func truncateUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
