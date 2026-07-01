package mcp

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// resolveServer turns a (possibly bare) server config into a full launch spec,
// in tiers: smithery → explicit → static catalog → MCP registry (auto-config an
// UNKNOWN server from registry.modelcontextprotocol.io). Any server a user
// references gets configured automatically.
func (m *Module) resolveServer(ctx context.Context, id string, sc schema.MCPServerConfig, autoInstall bool) (connectSpec, bool) {
	return resolveServerSpec(ctx, id, sc, autoInstall)
}

// resolveServerSpec is the receiver-free resolution shared by the runtime path
// and the management install resolver, so an installed server connects exactly
// like a runtime-resolved one.
func resolveServerSpec(ctx context.Context, id string, sc schema.MCPServerConfig, autoInstall bool) (connectSpec, bool) {
	if sc.Via == "smithery" {
		spec, err := resolveSmithery(id, sc)
		if err != nil {
			return connectSpec{}, false
		}
		return spec, true
	}
	if sc.Command != "" || sc.URL != "" || sc.Transport != "" {
		return toConnectSpec(sc)
	}
	if entry, ok := catalogLookup(id); ok {
		return resolveFromCatalog(entry, sc, autoInstall), true
	}
	// Tier 5 — the official MCP registry: an unknown server is fetched and
	// mapped to a launch config, with the user's shorthand credentials wired to
	// the server's declared env vars and OAuth auto-detected from those names.
	if srv, ok := searchRegistry(ctx, id); ok {
		if spec, _, ok := registryToConnectSpec(srv, sc); ok {
			return spec, true
		}
	}
	return connectSpec{}, false
}

func resolveFromCatalog(entry catalogEntry, sc schema.MCPServerConfig, autoInstall bool) connectSpec {
	transport := entry.Transport
	if transport == "" {
		transport = "stdio"
	}
	args := append([]string{}, entry.Args...)
	env := map[string]string{}
	for k, v := range entry.DefaultEnv {
		env[k] = v
	}
	for key, val := range sc.Extra {
		mapping, ok := entry.EnvMapping[key]
		if !ok {
			continue
		}
		s := fmt.Sprintf("%v", val)
		if mapping == argAppend {
			args = append(args, s)
		} else {
			env[mapping] = s
		}
	}
	for k, v := range sc.Env {
		env[k] = v
	}
	if len(sc.Args) > 0 {
		args = append([]string{}, sc.Args...)
	}

	timeout := defaultTimeout
	if entry.Timeout > 0 {
		timeout = time.Duration(entry.Timeout * float64(time.Second))
	}
	if sc.Timeout > 0 {
		timeout = time.Duration(sc.Timeout * float64(time.Second))
	}

	spec := connectSpec{
		Transport: transport,
		Command:   entry.Command,
		Args:      args,
		Env:       env,
		Headers:   sc.Headers,
		Timeout:   timeout,
	}
	applyRuntime(entry, &spec, autoInstall)
	return spec
}

func applyRuntime(entry catalogEntry, spec *connectSpec, autoInstall bool) {
	if entry.Runtime != "pip" || entry.Package == "" {
		return
	}
	if _, err := exec.LookPath(entry.Command); err == nil {
		return
	}
	if uvx, err := exec.LookPath("uvx"); err == nil {
		spec.Command = uvx
		spec.Args = append([]string{entry.Package}, spec.Args...)
		return
	}
	if autoInstall {
		_ = tryAutoInstall(entry.Package)
	}
}
