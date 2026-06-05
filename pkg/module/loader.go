package module

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// ManifestFile is the on-disk representation of a module manifest.
type ManifestFile struct {
	ID                   string               `yaml:"id"`
	Version              string               `yaml:"version,omitempty"`
	Description          string               `yaml:"description,omitempty"`
	SupportedPlatforms   []string             `yaml:"supported_platforms,omitempty"`
	Dependencies         []string             `yaml:"dependencies,omitempty"`
	DeclaredPermissions  []string             `yaml:"declared_permissions,omitempty"`
	ProvidesServices     []string             `yaml:"provides_services,omitempty"`
	ConsumesServices     []string             `yaml:"consumes_services,omitempty"`
	Constraints          []ManifestConstraint `yaml:"constraints,omitempty"`
	ConfigSchema         map[string]any       `yaml:"config_schema,omitempty"`
	CompatibleMiddleware []string             `yaml:"compatible_middleware,omitempty"`
	Tools                []ManifestTool       `yaml:"tools"`
}

type ManifestTool struct {
	Name               string          `yaml:"name"`
	Description        string          `yaml:"description"`
	RiskLevel          string          `yaml:"risk_level,omitempty"`
	Permissions        []string        `yaml:"permissions,omitempty"`
	Irreversible       bool            `yaml:"irreversible,omitempty"`
	RequireApproval    bool            `yaml:"require_approval,omitempty"`
	Internal           bool            `yaml:"internal,omitempty"`
	ToolPrompt         string          `yaml:"tool_prompt,omitempty"`
	DataClassification string          `yaml:"data_classification,omitempty"`
	CLILabel           string          `yaml:"cli_label,omitempty"`
	CLIParam           string          `yaml:"cli_param,omitempty"`
	Tags               []string        `yaml:"tags,omitempty"`
	Aliases            []string        `yaml:"aliases,omitempty"`
	Params             []ManifestParam `yaml:"params,omitempty"`
}

type ManifestParam struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Description string `yaml:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty"`
	Default     any    `yaml:"default,omitempty"`
	Enum        []any  `yaml:"enum,omitempty"`
	Path        bool   `yaml:"path,omitempty"`
}

type ManifestConstraint struct {
	Name        string   `yaml:"name"`
	Type        string   `yaml:"type"`
	Description string   `yaml:"description,omitempty"`
	Scope       string   `yaml:"scope,omitempty"`
	Default     any      `yaml:"default,omitempty"`
	AppliesTo   []string `yaml:"applies_to,omitempty"`
}

func (f ManifestFile) ToManifest() Manifest {
	platforms := make([]Platform, len(f.SupportedPlatforms))
	for i, p := range f.SupportedPlatforms {
		platforms[i] = Platform(p)
	}
	tools := make([]ToolSpec, len(f.Tools))
	for i, t := range f.Tools {
		tools[i] = t.toSpec()
	}
	return Manifest{
		ID:                   f.ID,
		Version:              f.Version,
		Description:          f.Description,
		SupportedPlatforms:   platforms,
		Dependencies:         f.Dependencies,
		DeclaredPermissions:  f.DeclaredPermissions,
		ProvidesServices:     f.ProvidesServices,
		ConsumesServices:     f.ConsumesServices,
		Tools:                tools,
		ConfigSchema:         f.ConfigSchema,
		CompatibleMiddleware: f.CompatibleMiddleware,
	}
}

func (t ManifestTool) toSpec() ToolSpec {
	params := make([]ParamSpec, len(t.Params))
	for i, p := range t.Params {
		params[i] = ParamSpec{
			Name:        p.Name,
			Type:        p.Type,
			Description: p.Description,
			Required:    p.Required,
			Default:     p.Default,
			Enum:        p.Enum,
			Path:        p.Path,
		}
	}
	return ToolSpec{
		Name:               t.Name,
		Description:        t.Description,
		Params:             params,
		Permissions:        t.Permissions,
		RiskLevel:          tool.RiskLevel(t.RiskLevel),
		Irreversible:       t.Irreversible,
		ToolPrompt:         t.ToolPrompt,
		DataClassification: t.DataClassification,
		Internal:           t.Internal,
		CLILabel:           t.CLILabel,
		CLIParam:           t.CLIParam,
	}
}

// LoadManifestFile reads one YAML manifest from disk.
func LoadManifestFile(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("load manifest %s: %w", path, err)
	}
	var f ManifestFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if f.ID == "" {
		return Manifest{}, fmt.Errorf("manifest %s: missing id", path)
	}
	return f.ToManifest(), nil
}

// LoadManifestDir scans a directory for *.yaml / *.yml manifest files and
// returns the parsed manifests sorted by ID. Missing directories are not an
// error — they return an empty slice.
func LoadManifestDir(dir string) ([]Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan %s: %w", dir, err)
	}
	out := make([]Manifest, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		m, err := LoadManifestFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Ensure the platform-helper compiles against domain types we re-export.
var _ = domainmodule.PlatformLinux
