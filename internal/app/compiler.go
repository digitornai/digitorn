// Package app compiles a YAML application manifest into a validated
// in-memory app.Definition that the runtime can execute.
package app

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	domainapp "github.com/mbathepaul/digitorn/internal/domain/app"
)

// Compiler loads, validates, and resolves variables in app YAML manifests.
type Compiler struct {
	// Env is the env-var lookup function used by {{env.X}} placeholders. Defaults to os.Getenv.
	Env func(string) string
	// Vars are additional variables for {{var.X}} placeholders.
	Vars map[string]string
}

// NewCompiler creates a compiler with default env lookup.
func NewCompiler() *Compiler {
	return &Compiler{Env: os.Getenv, Vars: map[string]string{}}
}

// CompileFile reads a YAML file from disk and compiles it.
func (c *Compiler) CompileFile(path string) (*domainapp.Definition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("compiler: read %s: %w", path, err)
	}
	return c.Compile(raw)
}

// Compile parses and validates a YAML manifest from bytes.
func (c *Compiler) Compile(raw []byte) (*domainapp.Definition, error) {
	// 1. Variable substitution
	expanded, err := ResolveVariables(string(raw), c.Env, c.Vars)
	if err != nil {
		return nil, fmt.Errorf("compiler: resolve vars: %w", err)
	}

	// 2. YAML parse
	var def domainapp.Definition
	if err := yaml.Unmarshal([]byte(expanded), &def); err != nil {
		return nil, fmt.Errorf("compiler: yaml unmarshal: %w", err)
	}

	// 3. Schema validation
	if err := validate(&def); err != nil {
		return nil, fmt.Errorf("compiler: validate: %w", err)
	}

	// 4. Cross-reference validation
	if err := validateReferences(&def); err != nil {
		return nil, fmt.Errorf("compiler: refs: %w", err)
	}

	return &def, nil
}

func validate(def *domainapp.Definition) error {
	if def.App.AppID == "" {
		return fmt.Errorf("app.app_id is required")
	}
	if def.App.Name == "" {
		return fmt.Errorf("app.name is required")
	}
	if len(def.Agents) == 0 {
		return fmt.Errorf("at least one agent must be declared")
	}
	for i, a := range def.Agents {
		if a.ID == "" {
			return fmt.Errorf("agents[%d].id is required", i)
		}
		if a.Brain.Provider == "" {
			return fmt.Errorf("agents[%d].brain.provider is required", i)
		}
		if a.Brain.Model == "" {
			return fmt.Errorf("agents[%d].brain.model is required", i)
		}
	}
	return nil
}

func validateReferences(def *domainapp.Definition) error {
	agentIDs := make(map[string]struct{}, len(def.Agents))
	for _, a := range def.Agents {
		agentIDs[a.ID] = struct{}{}
	}
	for i, a := range def.Agents {
		for _, target := range a.DelegateTo {
			if _, ok := agentIDs[target]; !ok {
				return fmt.Errorf("agents[%d].delegate_to references unknown agent %q", i, target)
			}
		}
	}
	return nil
}
