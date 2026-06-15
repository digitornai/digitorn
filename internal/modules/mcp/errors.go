package mcp

import (
	"fmt"
	"regexp"
	"strings"
)

// MCPTransportError is the single transport/protocol failure type (tool failures
// are not errors — they return a result with IsError set).
type MCPTransportError struct {
	Message   string
	Code      int
	Retryable bool
}

func (e *MCPTransportError) Error() string { return e.Message }

func transportErr(format string, args ...any) *MCPTransportError {
	return &MCPTransportError{Message: fmt.Sprintf(format, args...), Code: -1, Retryable: true}
}

// httpStatusRe pulls a 3-digit HTTP status out of an SDK/transport error string
// (the go-sdk surfaces http failures as "... status 401 ...").
var httpStatusRe = regexp.MustCompile(`\b([45]\d\d)\b`)

// nonRetryableHints are substrings that mark a PERMANENT failure: the server
// will never recover by retrying (bad auth, missing route, missing binary, bad
// request). Anything else (connection refused, reset, timeout, EOF) is transient
// and retryable.
var nonRetryableHints = []string{
	"unauthorized", "forbidden", "not found", "bad request",
	"invalid_grant", "invalid request", "unprocessable",
	"executable file not found", "no such file", "command not found",
	"unsupported protocol version",
}

// classify maps any error to an MCPTransportError, deciding retryability so the
// reconnect/health loop backs off transient faults but STOPS hammering a server
// that can never recover (auth failure, 404, missing command). Previously every
// error was marked retryable, which made a permanently-broken server reconnect
// every cycle forever.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if te, ok := err.(*MCPTransportError); ok {
		return te
	}
	msg := err.Error()
	low := strings.ToLower(msg)

	code := -1
	retryable := true
	if m := httpStatusRe.FindString(msg); m != "" {
		fmt.Sscanf(m, "%d", &code)
		// 4xx (except 408 timeout / 429 rate-limit) are client errors — not
		// retryable. 5xx are server-side and may recover, so stay retryable.
		if code >= 400 && code < 500 && code != 408 && code != 429 {
			retryable = false
		}
	}
	for _, h := range nonRetryableHints {
		if strings.Contains(low, h) {
			retryable = false
			break
		}
	}
	return &MCPTransportError{Message: msg, Code: code, Retryable: retryable}
}
