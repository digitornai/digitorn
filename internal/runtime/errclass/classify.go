// Package errclass turns a turn-failure error into the structured shape the
// web/flutter clients parse from the daemon's `error` event (DaemonError:
// error/code/category/retry/detail). It mirrors the legacy daemon's
// _classify_error so existing clients render and recover identically. Pure and
// allocation-light: it runs ONLY on the failure path, never on a successful turn.
package errclass

import "strings"

type Info struct {
	Error    string
	Code     string
	Category string
	Detail   string
	Retry    bool
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func anyOf(s string, kws ...string) bool {
	for _, k := range kws {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// Classify maps err to the client error contract. Keyword order matches the
// legacy daemon so the same upstream message yields the same code/category.
func Classify(err error) Info {
	if err == nil {
		return Info{Error: "An unexpected error occurred.", Code: "internal_error", Category: "internal", Retry: true}
	}
	msg := err.Error()
	detail := clip(msg, 500)
	low := strings.ToLower(msg)

	switch {
	case strings.Contains(low, "model_not_provided_by_digitorn") || strings.Contains(low, "is not provided by digitorn"):
		return Info{
			Error:    "This model is not provided by Digitorn. Configure your own credentials for this provider, or use a supported model.",
			Code:     "model_not_provided_by_digitorn", Category: "configuration", Retry: false, Detail: detail,
		}
	case anyOf(low, "insufficient", "quota", "balance", "billing", "payment", "402", "exceeded your current quota", "budget"):
		return Info{Error: "Insufficient balance or quota exceeded. Please check your API billing.", Code: "insufficient_balance", Category: "billing", Retry: false, Detail: detail}
	case anyOf(low, "401", "unauthorized", "invalid api key", "api key", "token expired", "forbidden", "403", "authentication"):
		return Info{Error: "Authentication failed. Check your API key or token.", Code: "auth_error", Category: "auth", Retry: false, Detail: detail}
	case anyOf(low, "rate limit", "429", "too many requests", "throttl"):
		return Info{Error: "Rate limited by the provider. Please wait a moment.", Code: "rate_limited", Category: "rate_limit", Retry: true, Detail: detail}
	case anyOf(low, "context length", "context window", "maximum context", "token limit", "too long"):
		return Info{Error: "Message too long for the model's context window.", Code: "context_overflow", Category: "provider", Retry: false, Detail: detail}
	// Specific internal causes BEFORE the generic network case : their messages
	// ("session lock timeout") contain network keywords that would otherwise win.
	case strings.Contains(low, "lock timeout") || strings.Contains(low, "session lock"):
		return Info{Error: "Another turn is still running on this session. Wait for it to finish.", Code: "session_busy", Category: "internal", Retry: true, Detail: detail}
	case strings.Contains(low, "loop_guard_hard_kill") || strings.Contains(low, "turn aborted to prevent runaway"):
		return Info{Error: "The agent got stuck calling the same broken tool. The turn was stopped to prevent runaway. Rephrase or start a fresh session.", Code: "agent_loop_killed", Category: "internal", Retry: true, Detail: detail}
	case strings.Contains(low, "no progress") || strings.Contains(low, "safety window") || strings.Contains(low, "safety cutoff"):
		return Info{Error: "The turn stalled — a tool or model call made no progress within the safety window.", Code: "turn_stalled", Category: "internal", Retry: true, Detail: detail}
	case anyOf(low, "connection", "timeout", "timed out", "unreachable", "dns", "ssl", "eof", "reset by peer", "connection refused"):
		return Info{Error: "Network error connecting to the AI provider.", Code: "network_error", Category: "network", Retry: true, Detail: detail}
	case anyOf(low, "500", "502", "503", "504", "server error", "internal server"):
		return Info{Error: "The AI provider returned a server error. Try again.", Code: "provider_error", Category: "provider", Retry: true, Detail: detail}
	case strings.Contains(low, "permission denied") || strings.Contains(low, "permission"):
		return Info{Error: "Permission denied: " + clip(msg, 200), Code: "permission_denied", Category: "security", Retry: false, Detail: detail}
	default:
		return Info{Error: clip(msg, 500), Code: "internal_error", Category: "internal", Retry: true, Detail: detail}
	}
}
