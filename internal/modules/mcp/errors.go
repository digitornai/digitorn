package mcp

import (
	"fmt"
	"regexp"
	"strings"
)

type MCPTransportError struct {
	Message   string
	Code      int
	Retryable bool
}

func (e *MCPTransportError) Error() string { return e.Message }

func transportErr(format string, args ...any) *MCPTransportError {
	return &MCPTransportError{Message: fmt.Sprintf(format, args...), Code: -1, Retryable: true}
}

var httpStatusRe = regexp.MustCompile(`\b([45]\d\d)\b`)

var nonRetryableHints = []string{
	"unauthorized", "forbidden", "not found", "bad request",
	"invalid_grant", "invalid request", "unprocessable",
	"executable file not found", "no such file", "command not found",
	"unsupported protocol version",
}

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
