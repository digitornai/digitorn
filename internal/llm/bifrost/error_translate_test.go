package bifrost

import (
	"testing"

	schemas "github.com/maximhq/bifrost/core/schemas"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestHTTPStatusToGRPCCode verifies the full mapping table. Every
// row in httpStatusToGRPCCode gets a case + a default-case sanity check.
//
// Why this matters: the daemon's `client.go::isRetryable` switches on
// gRPC code. A wrong mapping here = wrong retry behaviour in prod
// (e.g. silently retrying a 401 or NOT retrying a 503).
func TestHTTPStatusToGRPCCode(t *testing.T) {
	cases := []struct {
		http int
		want codes.Code
		why  string
	}{
		{400, codes.InvalidArgument, "bad request — don't retry, fix the caller"},
		{401, codes.Unauthenticated, "fix creds, don't retry"},
		{402, codes.FailedPrecondition, "billing — surface clearly"},
		{403, codes.PermissionDenied, "wrong scopes, don't retry"},
		{404, codes.NotFound, "model/provider misconfigured"},
		{408, codes.DeadlineExceeded, "retry"},
		{409, codes.Aborted, "retry on concurrent-write conflict"},
		{422, codes.InvalidArgument, "validation"},
		{429, codes.ResourceExhausted, "upstream rate-limit, retry with backoff"},
		{499, codes.Canceled, "client gave up"},
		{500, codes.Unavailable, "retry"},
		{501, codes.Unimplemented, "config issue, don't retry"},
		{502, codes.Unavailable, "retry"},
		{503, codes.Unavailable, "retry"},
		{504, codes.Unavailable, "retry"},
		{599, codes.Unavailable, "any other 5xx → unavailable"},
		{0, codes.Unknown, "no status code captured"},
		{199, codes.Unknown, "1xx is nonsense, defensive"},
		{200, codes.Unknown, "200 should never be an error path; map defensively"},
		{300, codes.Unknown, "3xx redirects shouldn't surface as errors"},
		{600, codes.Unknown, "absurd code"},
	}
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			got := httpStatusToGRPCCode(c.http)
			if got != c.want {
				t.Errorf("httpStatusToGRPCCode(%d) = %v, want %v", c.http, got, c.want)
			}
		})
	}
}

// TestTranslateError_PreservesStatusCode confirms the gRPC code on the
// returned error matches the upstream HTTP status. Catches the historical
// bug where every error became codes.Unknown.
func TestTranslateError_PreservesStatusCode(t *testing.T) {
	cases := []struct {
		name           string
		httpStatus     int
		wantGRPCCode   codes.Code
		wantRetryable  bool // documents intent; not asserted here
	}{
		{"400_invalid_request", 400, codes.InvalidArgument, false},
		{"401_auth", 401, codes.Unauthenticated, false},
		{"403_forbidden", 403, codes.PermissionDenied, false},
		{"429_rate_limit", 429, codes.ResourceExhausted, true},
		{"500_internal", 500, codes.Unavailable, true},
		{"503_unavailable", 503, codes.Unavailable, true},
		{"504_gateway_timeout", 504, codes.Unavailable, true},
		{"no_status", 0, codes.Unknown, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			berr := &schemas.BifrostError{
				StatusCode: ptrInt(c.httpStatus),
				Error: &schemas.ErrorField{Message: "test error msg"},
			}
			ec := errCallContext{
				Provider: "test_provider",
				Model:    "test_model",
			}
			err := translateError(berr, ec)
			if err == nil {
				t.Fatal("translateError returned nil for a non-nil bifrost error")
			}
			st, ok := status.FromError(err)
			if !ok {
				t.Fatalf("translateError did not return a *status.Status; got %T", err)
			}
			if st.Code() != c.wantGRPCCode {
				t.Errorf("gRPC code = %v, want %v", st.Code(), c.wantGRPCCode)
			}
		})
	}
}

// TestTranslateError_ErrorInfoDetail confirms the gRPC error carries an
// errdetails.ErrorInfo with provider + model + status_code metadata.
// Operators / dashboards rely on this to group failures correctly.
func TestTranslateError_ErrorInfoDetail(t *testing.T) {
	berr := &schemas.BifrostError{
		StatusCode: ptrInt(400),
		Error:      &schemas.ErrorField{Message: "missing field content"},
	}
	ec := errCallContext{
		Provider:      "anthropic",
		Model:         "claude-sonnet-4-7",
		CorrelationID: "corr-123",
		SessionID:     "sess-456",
		UserID:        "user-789",
		AgentID:       "agent-abc",
	}
	err := translateError(berr, ec)
	st, ok := status.FromError(err)
	if !ok {
		t.Fatal("expected *status.Status")
	}
	if len(st.Details()) == 0 {
		t.Fatal("expected at least one detail proto attached")
	}
	var info *errdetails.ErrorInfo
	for _, d := range st.Details() {
		if ei, ok := d.(*errdetails.ErrorInfo); ok {
			info = ei
			break
		}
	}
	if info == nil {
		t.Fatal("expected an errdetails.ErrorInfo detail")
	}
	if info.Reason != "anthropic_400" {
		t.Errorf("Reason = %q, want %q", info.Reason, "anthropic_400")
	}
	if info.Domain != "bifrost" {
		t.Errorf("Domain = %q, want %q", info.Domain, "bifrost")
	}
	mustMeta := map[string]string{
		"provider":       "anthropic",
		"model":          "claude-sonnet-4-7",
		"status_code":    "400",
		"correlation_id": "corr-123",
		"session_id":     "sess-456",
		"user_id":        "user-789",
		"agent_id":       "agent-abc",
	}
	for k, v := range mustMeta {
		if info.Metadata[k] != v {
			t.Errorf("Metadata[%q] = %q, want %q", k, info.Metadata[k], v)
		}
	}
}

// TestTranslateError_NilSafe makes sure the error path doesn't panic
// when berr is unexpectedly nil (defensive: a Bifrost contract change
// could regress this).
func TestTranslateError_NilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("translateError(nil, ...) panicked: %v", r)
		}
	}()
	err := translateError(nil, errCallContext{Provider: "x", Model: "y"})
	if err == nil {
		t.Fatal("expected a non-nil error even for nil berr")
	}
}

// TestErrStatusCode covers the nil-defensive branches that the contract
// tests above don't reach directly.
func TestErrStatusCode(t *testing.T) {
	if got := errStatusCode(nil); got != 0 {
		t.Errorf("errStatusCode(nil) = %d, want 0", got)
	}
	if got := errStatusCode(&schemas.BifrostError{}); got != 0 {
		t.Errorf("errStatusCode(empty) = %d, want 0", got)
	}
	if got := errStatusCode(&schemas.BifrostError{StatusCode: ptrInt(429)}); got != 429 {
		t.Errorf("errStatusCode(429) = %d, want 429", got)
	}
}

func ptrInt(v int) *int { return &v }
