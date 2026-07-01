// Package projectsettings loads per-project overrides from .digitorn/settings.yaml.
// Only the permissions block is supported in phase 1. The file is optional;
// a missing file is silently ignored. Nothing in this package mutates shared
// app state — it always returns a fresh CapabilitiesConfig to merge.
package projectsettings

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

var writeMu sync.Mutex

const FileName = ".digitorn/settings.yaml"

type Settings struct {
	Permissions PermissionsBlock `yaml:"permissions"`
}

type PermissionsBlock struct {
	Allow   []string `yaml:"allow,omitempty"`
	Deny    []string `yaml:"deny,omitempty"`
	Approve []string `yaml:"approve,omitempty"`
}

// Load reads .digitorn/settings.yaml from workdir. Returns nil, nil when the
// file is absent (no override). Returns an error only on malformed YAML.
func Load(workdir string) (*Settings, error) {
	if workdir == "" {
		return nil, nil
	}
	path := filepath.Join(workdir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil // missing = no override
	}
	var s Settings
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Capabilities converts the permissions block into a CapabilitiesConfig that
// can be merged with the app-level caps. Returns nil when no permissions are
// declared.
func (s *Settings) Capabilities() *schema.CapabilitiesConfig {
	if s == nil {
		return nil
	}
	p := s.Permissions
	if len(p.Allow)+len(p.Deny)+len(p.Approve) == 0 {
		return nil
	}
	return &schema.CapabilitiesConfig{
		Grant:   parseGrants(p.Allow),
		Deny:    parseGrants(p.Deny),
		Approve: parseGrants(p.Approve),
	}
}

// Allow appends a signature to permissions.allow in .digitorn/settings.yaml,
// creating the file if it doesn't exist. Thread-safe.
func Allow(workdir, signature string) error {
	if workdir == "" || signature == "" {
		return nil
	}
	writeMu.Lock()
	defer writeMu.Unlock()

	path := filepath.Join(workdir, FileName)

	var s Settings
	if data, err := os.ReadFile(path); err == nil {
		_ = yaml.Unmarshal(data, &s)
	}
	for _, v := range s.Permissions.Allow {
		if strings.TrimSpace(v) == signature {
			return nil
		}
	}
	s.Permissions.Allow = append(s.Permissions.Allow, signature)

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := yaml.Marshal(&s)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// parseGrants converts ["bash.run", "filesystem.write"] into CapabilityGrant
// slices. Entries without a dot are treated as module-level (all tools).
// Parenthetical argument patterns like "bash.run(go test *)" are accepted but
// the filter part is stripped — argument-level matching is not yet implemented.
func parseGrants(entries []string) []schema.CapabilityGrant {
	if len(entries) == 0 {
		return nil
	}
	out := make([]schema.CapabilityGrant, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		// Strip argument patterns: "bash.run(go test *)" → "bash.run"
		if i := strings.Index(e, "("); i > 0 {
			e = e[:i]
		}
		// Strip arg suffix from signatures: "bash.run:go test ./..." → "bash.run"
		if i := strings.Index(e, ":"); i > 0 {
			e = e[:i]
		}
		if dot := strings.LastIndex(e, "."); dot > 0 {
			out = append(out, schema.CapabilityGrant{
				Module: e[:dot],
				Tools:  []string{e[dot+1:]},
			})
		} else {
			out = append(out, schema.CapabilityGrant{Module: e})
		}
	}
	return out
}
