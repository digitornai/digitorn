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

func Load(workdir string) (*Settings, error) {
	if workdir == "" {
		return nil, nil
	}
	path := filepath.Join(workdir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var s Settings
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

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
		if i := strings.Index(e, "("); i > 0 {
			e = e[:i]
		}
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
