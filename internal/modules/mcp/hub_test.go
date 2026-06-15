package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/mcphub"
)

func TestResolveInstallHub_SplitsSecrets(t *testing.T) {
	e := mcphub.FeaturedEntry{
		ServerID:    "github",
		DisplayName: "GitHub",
		Transport:   "stdio",
		Command:     "npx",
		Args:        []string{"-y", "@modelcontextprotocol/server-github"},
		Runtime:     "npm",
		Package:     "@modelcontextprotocol/server-github",
		EnvMapping:  map[string]string{"token": "GITHUB_PERSONAL_ACCESS_TOKEN"},
	}
	res, ok := ResolveInstallHub(context.Background(), e, map[string]string{"token": "ghp_x"})
	if !ok {
		t.Fatal("resolve failed")
	}
	if res.Source != "hub" {
		t.Fatalf("source=%q want hub", res.Source)
	}
	if res.Command != "npx" {
		t.Fatalf("command=%q", res.Command)
	}
	// The credential value must land in secrets (sealed later), NOT plain env.
	if res.Secrets["GITHUB_PERSONAL_ACCESS_TOKEN"] != "ghp_x" {
		t.Fatalf("secret not mapped: %#v", res.Secrets)
	}
	if _, leaked := res.Env["GITHUB_PERSONAL_ACCESS_TOKEN"]; leaked {
		t.Fatalf("credential leaked into non-secret env: %#v", res.Env)
	}
	if res.AuthType != "token" {
		t.Fatalf("auth_type=%q want token", res.AuthType)
	}
}

func TestResolveInstallHub_OAuthAuthType(t *testing.T) {
	e := mcphub.FeaturedEntry{
		ServerID: "notion", Transport: "stdio", Command: "mcp-notion",
		OAuthProvider: "notion",
	}
	res, ok := ResolveInstallHub(context.Background(), e, nil)
	if !ok || res.AuthType != "oauth2" {
		t.Fatalf("ok=%v auth_type=%q want oauth2", ok, res.AuthType)
	}
}

func TestResolveInstallHub_Hosted(t *testing.T) {
	hosted := "https://hosted.digitorn.ai/mcp/slack"
	e := mcphub.FeaturedEntry{
		ServerID: "slack", DisplayName: "Slack", Transport: "stdio", Command: "npx",
		HostedURL:        &hosted,
		DigitornProvided: map[string]string{"Authorization": "Bearer dgtn-managed"},
	}
	res, ok := ResolveInstallHub(context.Background(), e, nil) // no user creds
	if !ok {
		t.Fatal("hosted resolve failed")
	}
	if res.Transport != "streamable_http" || res.URL != hosted {
		t.Fatalf("hosted should be remote: transport=%q url=%q", res.Transport, res.URL)
	}
	if res.AuthType != "hosted" {
		t.Fatalf("auth_type=%q want hosted", res.AuthType)
	}
	// digitorn_provided becomes the (sealed) secret → request header at dial time.
	if res.Secrets["Authorization"] != "Bearer dgtn-managed" {
		t.Fatalf("digitorn_provided not carried: %#v", res.Secrets)
	}
}

func TestHubCatalogInfo_TrustFlags(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	hosted := "https://hosted.example/mcp"
	e := mcphub.FeaturedEntry{
		ServerID: "slack", DisplayName: "Slack", Transport: "stdio", Command: "npx",
		VerifiedAt: &now, HostedURL: &hosted, Icon: "🔌", Category: "communication",
	}
	info := HubCatalogInfo(e)
	if info.Source != "hub" || !info.Verified || !info.Hosted {
		t.Fatalf("flags wrong: source=%q verified=%v hosted=%v", info.Source, info.Verified, info.Hosted)
	}
	if info.Icon != "🔌" || info.Category != "communication" {
		t.Fatalf("icon/category lost: %+v", info)
	}
}

func TestHubRequirements_KeyDescriptions(t *testing.T) {
	e := mcphub.FeaturedEntry{
		ServerID:        "github",
		Transport:       "stdio",
		Command:         "npx",
		EnvMapping:      map[string]string{"token": "GITHUB_PERSONAL_ACCESS_TOKEN"},
		KeyDescriptions: map[string]string{"token": "Your PAT from GitHub settings"},
	}
	req := HubRequirements(e)
	if req.Source != "hub" || len(req.Credentials) != 1 {
		t.Fatalf("req=%+v", req)
	}
	if req.Credentials[0].Description != "Your PAT from GitHub settings" {
		t.Fatalf("key_description not applied: %q", req.Credentials[0].Description)
	}
}
