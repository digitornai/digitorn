package mcpoauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// dcrRequest is the RFC 7591 client-registration request body.
type dcrRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope,omitempty"`
}

// dcrResponse is the subset of the RFC 7591 registration response we keep.
type dcrResponse struct {
	ClientID                string `json:"client_id"`
	ClientSecret            string `json:"client_secret"`
	TokenEndpointAuthMethod string `json:"token_endpoint_auth_method"`
}

// registerClient performs RFC 7591 Dynamic Client Registration: it asks the
// authorization server's registration endpoint for an authorization-code client
// bound to our single redirect URI. We request a public (PKCE) client; the AS may
// still issue a secret, which the caller stores. Returns the issued client.
func registerClient(ctx context.Context, client *http.Client, registrationEndpoint, clientName, redirectURI, scope string) (dcrResponse, error) {
	if registrationEndpoint == "" {
		return dcrResponse{}, fmt.Errorf("mcpoauth: authorization server offers no dynamic-registration endpoint")
	}
	buf, err := json.Marshal(dcrRequest{
		ClientName:              clientName,
		RedirectURIs:            []string{redirectURI},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scope:                   scope,
	})
	if err != nil {
		return dcrResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, bytes.NewReader(buf))
	if err != nil {
		return dcrResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return dcrResponse{}, fmt.Errorf("mcpoauth: dynamic client registration: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return dcrResponse{}, fmt.Errorf("mcpoauth: dynamic client registration: status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out dcrResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return dcrResponse{}, fmt.Errorf("mcpoauth: dynamic client registration: malformed response: %w", err)
	}
	if out.ClientID == "" {
		return dcrResponse{}, fmt.Errorf("mcpoauth: dynamic client registration returned no client_id")
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
