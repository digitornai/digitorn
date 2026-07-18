package llm

import (
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// statusWithUpstreamCode builds a gRPC error carrying the ErrorInfo
// detail bifrost.translateError attaches, so we can drive isRetryable
// exactly as the real error path does.
func statusWithUpstreamCode(c codes.Code, upstreamCode string) error {
	st := status.New(c, "bifrost: test")
	if upstreamCode != "" {
		st, _ = st.WithDetails(&errdetails.ErrorInfo{
			Reason:   "test",
			Domain:   "bifrost",
			Metadata: map[string]string{"upstream_code": upstreamCode},
		})
	}
	return st.Err()
}

func TestIsRetryable_QuotaNeverRetried(t *testing.T) {
	// The whole point: a quota block (429 → ResourceExhausted with
	// upstream_code=quota_exceeded) must NOT be retried.
	if isRetryable(statusWithUpstreamCode(codes.ResourceExhausted, "quota_exceeded")) {
		t.Fatal("quota_exceeded (429) must NOT be retryable")
	}
	// A genuine upstream provider rate-limit (429, no gateway quota code)
	// stays retryable — it can succeed after a backoff.
	if !isRetryable(statusWithUpstreamCode(codes.ResourceExhausted, "")) {
		t.Fatal("a bare provider 429 should still be retryable")
	}
	if !isRetryable(statusWithUpstreamCode(codes.ResourceExhausted, "rate_limited")) {
		t.Fatal("a non-quota 429 should still be retryable")
	}
}

func TestIsRetryable_TransientVsClient(t *testing.T) {
	retryable := []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Aborted}
	for _, c := range retryable {
		if !isRetryable(statusWithUpstreamCode(c, "")) {
			t.Fatalf("%v should be retryable", c)
		}
	}
	// Client-side errors are deterministic — retrying just wastes budget.
	clientSide := []codes.Code{
		codes.InvalidArgument, codes.Unauthenticated,
		codes.PermissionDenied, codes.NotFound, codes.Unimplemented,
	}
	for _, c := range clientSide {
		if isRetryable(statusWithUpstreamCode(c, "")) {
			t.Fatalf("%v must NOT be retryable", c)
		}
	}
	if isRetryable(nil) {
		t.Fatal("nil error must not be retryable")
	}
}

func TestRetryBackoff_MonotonicAndCapped(t *testing.T) {
	if got := retryBackoff(1); got.Milliseconds() != 200 {
		t.Fatalf("attempt 1: got %v, want 200ms", got)
	}
	if got := retryBackoff(2); got.Milliseconds() != 400 {
		t.Fatalf("attempt 2: got %v, want 400ms", got)
	}
	if got := retryBackoff(100); got.Seconds() != 5 {
		t.Fatalf("large attempt must cap at 5s, got %v", got)
	}
}
