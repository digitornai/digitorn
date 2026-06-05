package contextcompact

import (
	"errors"
	"testing"
)

func TestIsContextOverflow(t *testing.T) {
	overflow := []string{
		"This model's maximum context length is 8192 tokens",
		"error code: context_length_exceeded",
		"the context window has been exceeded",
		"Please reduce the length of the messages",
		"too many tokens in the request",
		"you hit the token limit",
		"prompt is too long: 250000 tokens",
	}
	for _, s := range overflow {
		if !IsContextOverflow(errors.New(s)) {
			t.Errorf("IsContextOverflow(%q) = false, want true", s)
		}
	}

	notOverflow := []string{"rate limited", "invalid api key", "connection refused", ""}
	for _, s := range notOverflow {
		if IsContextOverflow(errors.New(s)) {
			t.Errorf("IsContextOverflow(%q) = true, want false", s)
		}
	}
	if IsContextOverflow(nil) {
		t.Error("IsContextOverflow(nil) = true, want false")
	}
}

func TestEmergencyKeepRecent(t *testing.T) {
	cases := []struct{ in, want int }{
		{20, 10}, {10, 5}, {8, 4}, {6, 4}, {4, 4}, {0, 5}, // 0 → default 10 → half 5
	}
	for _, c := range cases {
		if got := EmergencyKeepRecent(c.in); got != c.want {
			t.Errorf("EmergencyKeepRecent(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
