package pieces

import (
	"errors"
	"testing"
)

func TestIsPermanentOAuthError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("refresh failed: 400 Bad Request: {\"error\":\"invalid_grant\"}"), true},
		{errors.New("400: invalid_token"), true},
		{errors.New("unauthorized_client"), true},
		{errors.New("dial tcp: connection refused"), false},
		{errors.New("refresh failed: 500 Internal Server Error"), false},
	}
	for _, c := range cases {
		if got := isPermanentOAuthError(c.err); got != c.want {
			t.Errorf("isPermanentOAuthError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
