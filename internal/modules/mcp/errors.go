package mcp

import "fmt"

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

func classify(err error) error {
	if err == nil {
		return nil
	}
	if te, ok := err.(*MCPTransportError); ok {
		return te
	}
	return &MCPTransportError{Message: err.Error(), Code: -1, Retryable: true}
}
