package mcp

import (
	"fmt"
	"os/exec"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

func (m *Module) resolveServer(id string, sc schema.MCPServerConfig, autoInstall bool) (connectSpec, bool) {
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
