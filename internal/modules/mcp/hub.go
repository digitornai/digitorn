package mcp

import (
	"context"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/mcphub"
)

// This file bridges the Hub's curated MCP catalog into the daemon's own
// resolution: a Hub featured entry has the same shape as the internal catalog
// entry, so it resolves into a launch spec EXACTLY like a static-catalog server.
// The Hub provides the config; the daemon installs it locally.

func featuredToCatalogEntry(e mcphub.FeaturedEntry) catalogEntry {
	return catalogEntry{
		DisplayName:              e.DisplayName,
		Description:              e.Description,
		Transport:                e.Transport,
		Command:                  e.Command,
		Args:                     e.Args,
		Runtime:                  e.Runtime,
		Package:                  e.Package,
		EnvMapping:               e.EnvMapping,
		DefaultEnv:               e.DefaultEnv,
		OAuthProvider:            e.OAuthProvider,
		OAuthEnvTokenVar:         e.OAuthEnvTokenVar,
		OAuthScopes:              e.OAuthScopes,
		OAuthStyle:               e.OAuthStyle,
		OAuthKeyfileEnv:          e.OAuthKeyfileEnv,
		OAuthCredentialsEnv:      e.OAuthCredentialsEnv,
		OAuthCredentialsFilename: e.OAuthCredentialsFilename,
		SmitherySlug:             e.SmitherySlug,
		BinaryName:               e.BinaryName,
		Timeout:                  e.Timeout,
	}
}

// HubCatalogInfo maps a Hub featured entry to the catalog view, carrying the
// trust signals (verified / hosted) the static catalog can't have.
func HubCatalogInfo(e mcphub.FeaturedEntry) CatalogInfo {
	info := catalogInfoOf(e.ServerID, featuredToCatalogEntry(e))
	info.Source = "hub"
	info.Verified = e.Verified()
	info.Hosted = e.Hosted()
	info.Icon = e.Icon
	info.Category = e.Category
	return info
}

// HubRequirements derives the install requirements from a Hub featured entry,
// enriching each credential with the Hub's key_descriptions (the connect-form
// labels) — richer than the static catalog can offer.
func HubRequirements(e mcphub.FeaturedEntry) ServerRequirements {
	entry := featuredToCatalogEntry(e)
	req := requirementsFromCatalog(e.ServerID, entry)
	req.Source = "hub"
	for i := range req.Credentials {
		if d := e.KeyDescriptions[req.Credentials[i].Key]; d != "" {
			req.Credentials[i].Description = d
		}
	}
	return req
}

// ResolveInstallHub resolves a Hub featured entry + the user's shorthand
// credentials into a storable managed-server spec — separating credential
// values (to seal) from non-secret config, the same way the static catalog
// install does. ok=false when the entry can't be resolved (empty / bad).
func ResolveInstallHub(ctx context.Context, e mcphub.FeaturedEntry, credentials map[string]string) (Resolution, bool) {
	if e.ServerID == "" {
		return Resolution{}, false
	}
	// Hosted: Digitorn runs the server and provides the keys, so the install is
	// a remote endpoint with NO user setup. digitorn_provided is sealed as
	// secrets (mapped to request headers at connect time for remote transport).
	if e.Hosted() {
		return Resolution{
			ServerID: e.ServerID, DisplayName: firstNonEmpty(e.DisplayName, e.ServerID), Source: "hub",
			Transport: "streamable_http", URL: *e.HostedURL,
			Secrets: e.DigitornProvided, AuthType: "hosted",
		}, true
	}
	entry := featuredToCatalogEntry(e)
	extra := make(map[string]any, len(credentials))
	for k, v := range credentials {
		extra[k] = v
	}
	spec := resolveFromCatalog(entry, schema.MCPServerConfig{Extra: extra}, false)

	// Credential env vars (to seal) = every non-arg env-var the catalog maps a
	// shorthand onto; everything else in the resolved env is non-secret config.
	secretVars := map[string]bool{}
	for _, envVar := range entry.EnvMapping {
		if envVar != argAppend {
			secretVars[envVar] = true
		}
	}
	env := map[string]string{}
	secrets := map[string]string{}
	for k, v := range spec.Env {
		if secretVars[k] {
			secrets[k] = v
		} else {
			env[k] = v
		}
	}
	authType := ""
	switch {
	case e.OAuthProvider != "":
		authType = "oauth2"
	case len(secrets) > 0:
		authType = "token"
	}
	return Resolution{
		ServerID: e.ServerID, DisplayName: firstNonEmpty(e.DisplayName, e.ServerID), Source: "hub",
		Transport: normTransport(spec.Transport), Command: spec.Command, Args: spec.Args, URL: spec.URL,
		Env: env, Secrets: secrets, AuthType: authType, Package: e.Package,
	}, true
}
