package errclass

import (
	"errors"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		msg          string
		wantCode     string
		wantCategory string
		wantRetry    bool
	}{
		{"Error code: 401 Unauthorized", "auth_error", "auth", false},
		{"invalid api key provided", "auth_error", "auth", false},
		{"insufficient balance for this request", "insufficient_balance", "billing", false},
		{"you exceeded your current quota", "insufficient_balance", "billing", false},
		{"429 Too Many Requests", "rate_limited", "rate_limit", true},
		{"this exceeds the model's context window", "context_overflow", "provider", false},
		{"connection refused dialing gateway", "network_error", "network", true},
		{"upstream 503 server error", "provider_error", "provider", true},
		{"permission denied: workdir policy", "permission_denied", "security", false},
		{"session lock timeout", "session_busy", "internal", true},
		{"turn safety cutoff exceeded (no progress)", "turn_stalled", "internal", true},
		{"the model 'x' is not provided by Digitorn", "model_not_provided_by_digitorn", "configuration", false},
		{"some totally unexpected thing", "internal_error", "internal", true},
	}
	for _, c := range cases {
		got := Classify(errors.New(c.msg))
		if got.Code != c.wantCode || got.Category != c.wantCategory || got.Retry != c.wantRetry {
			t.Errorf("Classify(%q) = {code:%q cat:%q retry:%v}, want {code:%q cat:%q retry:%v}",
				c.msg, got.Code, got.Category, got.Retry, c.wantCode, c.wantCategory, c.wantRetry)
		}
		if got.Error == "" || !strings.Contains(got.Detail, c.msg[:min(len(c.msg), 10)]) {
			t.Errorf("Classify(%q) missing human error/detail: %+v", c.msg, got)
		}
	}
	if got := Classify(nil); got.Code != "internal_error" {
		t.Errorf("Classify(nil) code = %q, want internal_error", got.Code)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
