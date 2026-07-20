package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestVercelExchangeCode(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/oauth/access_token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		gotForm = r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"vc_tok_123","team_id":"team_abc","token_type":"Bearer"}`))
	}))
	defer srv.Close()

	old := vercelAPIBase
	vercelAPIBase = srv.URL
	defer func() { vercelAPIBase = old }()

	const redirect = "https://auth.digitorn.ai/oauth/mcp/callback"
	tok, team, err := vercelExchangeCode(context.Background(), "cid", "csecret", "the_code", redirect)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok != "vc_tok_123" {
		t.Errorf("token = %q, want vc_tok_123", tok)
	}
	if team != "team_abc" {
		t.Errorf("team = %q, want team_abc", team)
	}
	for k, want := range map[string]string{
		"code": "the_code", "client_id": "cid", "client_secret": "csecret", "redirect_uri": redirect,
	} {
		if gotForm.Get(k) != want {
			t.Errorf("form[%s] = %q, want %q", k, gotForm.Get(k), want)
		}
	}
}

func TestVercelExchangeCode_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	old := vercelAPIBase
	vercelAPIBase = srv.URL
	defer func() { vercelAPIBase = old }()

	if _, _, err := vercelExchangeCode(context.Background(), "c", "s", "bad", "r"); err == nil {
		t.Fatal("expected error on non-200 token response")
	}
}
