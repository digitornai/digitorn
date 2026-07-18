package server

import (
	"net/http"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// llmErrorInfo is the decoded, client-facing shape of an LLM-pipeline
// failure. A gRPC error from the worker carries three layers we care
// about: the transport code (codes.ResourceExhausted…), and — attached
// as an errdetails.ErrorInfo proto by bifrost.translateError — the
// upstream HTTP status and the gateway's own application code
// ("quota_exceeded"). We flatten all three so the HTTP handler can pick
// the right status + hand the client a code it can branch on.
type llmErrorInfo struct {
	// HTTPStatus is what the web client should receive. Derived from the
	// gRPC code, NOT from the raw upstream status, so a retried-and-failed
	// 503 still reads as 502 to the browser (the gateway, not the browser,
	// owns provider retries).
	HTTPStatus int
	// Code is the machine-readable reason the UI branches on. Prefers the
	// gateway's application code ("quota_exceeded") over a generic
	// transport label so the composer can show "upgrade your plan" for a
	// quota block but "try again" for a transient provider outage.
	Code string
	// Message is the human-readable explanation, already composed upstream
	// (the gateway's quota message carries the retry date + upgrade hint).
	Message string
}

// classifyLLMError decodes a worker gRPC error into an llmErrorInfo.
// Falls back to a 502 "gateway_error" for anything it can't parse (a
// plain error, a nil status) so the handler always has something sane.
func classifyLLMError(err error) llmErrorInfo {
	out := llmErrorInfo{
		HTTPStatus: http.StatusBadGateway,
		Code:       "gateway_error",
		Message:    err.Error(),
	}
	st, ok := status.FromError(err)
	if !ok {
		return out
	}
	out.HTTPStatus = grpcCodeToHTTP(st.Code())
	if msg := st.Message(); msg != "" {
		out.Message = msg
	}
	// Mine the ErrorInfo detail for the gateway's application code + the
	// pre-composed message. This is the payload bifrost.translateError
	// attaches; its "upstream_code" is the gateway's error.code.
	for _, d := range st.Details() {
		info, ok := d.(*errdetails.ErrorInfo)
		if !ok {
			continue
		}
		if c := info.Metadata["upstream_code"]; c != "" {
			out.Code = c
		}
		if m := info.Metadata["message"]; m != "" {
			out.Message = m
		}
	}
	return out
}

// grpcCodeToHTTP is the inverse of bifrost.httpStatusToGRPCCode — it
// turns the transport code back into the HTTP status the web client
// expects. Kept deliberately in lockstep with that mapping so a
// quota block (429 → ResourceExhausted → 429) round-trips faithfully.
func grpcCodeToHTTP(c codes.Code) int {
	switch c {
	case codes.OK:
		return http.StatusOK
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.FailedPrecondition:
		return http.StatusPaymentRequired // 402: billing/precondition
	case codes.NotFound:
		return http.StatusNotFound
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests // 429: quota / rate-limit
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusBadGateway
	case codes.Canceled:
		return 499 // client closed request
	default:
		return http.StatusBadGateway
	}
}

// writeLLMError renders a worker error to the client with the right
// status + a stable machine-readable code. The body matches writeError's
// shape ({"error","message"}) plus the decoded code, so existing callers
// keep parsing it unchanged while quota-aware clients can branch on
// "code" === "quota_exceeded".
func (d *Daemon) writeLLMError(w http.ResponseWriter, err error) {
	info := classifyLLMError(err)
	writeJSON(w, info.HTTPStatus, map[string]any{
		"error":   info.Code,
		"message": info.Message,
	})
}
