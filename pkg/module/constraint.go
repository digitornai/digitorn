package module

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

type ConstraintType string

const (
	ConstraintString     ConstraintType = "string"
	ConstraintStringList ConstraintType = "string_list"
	ConstraintInteger    ConstraintType = "integer"
	ConstraintBoolean    ConstraintType = "boolean"
	ConstraintSize       ConstraintType = "size"     // e.g. "10MB"
	ConstraintDuration   ConstraintType = "duration" // e.g. "30s"
)

type ConstraintScope string

const (
	ConstraintScopeModule    ConstraintScope = "module"
	ConstraintScopeUniversal ConstraintScope = "universal"
)

type ConstraintSpec struct {
	Name        string
	Type        ConstraintType
	Description string
	Scope       ConstraintScope
	Default     any
	AppliesTo   []string
}

// ConstraintEnforcer applies one named constraint at every Invoke. The runtime
// attaches the constraint value map to the context (via WithConstraints); Base
// then iterates and calls each registered enforcer.
type ConstraintEnforcer interface {
	Name() string
	Check(ctx context.Context, moduleID, toolName string, params json.RawMessage, value any) error
}

// ErrConstraintViolation wraps a constraint failure with the offending name.
type ErrConstraintViolation struct {
	Constraint string
	Reason     string
}

func (e *ErrConstraintViolation) Error() string {
	return fmt.Sprintf("constraint %q violated: %s", e.Constraint, e.Reason)
}

type ConstraintRegistry struct {
	mu        sync.RWMutex
	enforcers map[string]ConstraintEnforcer
}

func NewConstraintRegistry() *ConstraintRegistry {
	return &ConstraintRegistry{enforcers: map[string]ConstraintEnforcer{}}
}

func (r *ConstraintRegistry) Register(e ConstraintEnforcer) {
	r.mu.Lock()
	r.enforcers[e.Name()] = e
	r.mu.Unlock()
}

func (r *ConstraintRegistry) Get(name string) (ConstraintEnforcer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.enforcers[name]
	return e, ok
}

// DefaultConstraints is the process-wide registry the Base consults when an
// Invoke arrives with a non-empty constraints map on its context.
var DefaultConstraints = NewConstraintRegistry()

func init() {
	DefaultConstraints.Register(allowedCommandsEnforcer{})
	DefaultConstraints.Register(blockedCommandsEnforcer{})
	DefaultConstraints.Register(allowedPathsEnforcer{})
	DefaultConstraints.Register(blockedActionsEnforcer{})
	DefaultConstraints.Register(allowedActionsEnforcer{})
}

func enforceConstraints(ctx context.Context, moduleID, toolName string, params json.RawMessage) error {
	cs := Constraints(ctx)
	if len(cs) == 0 {
		return nil
	}
	for name, value := range cs {
		e, ok := DefaultConstraints.Get(name)
		if !ok {
			continue
		}
		if err := e.Check(ctx, moduleID, toolName, params, value); err != nil {
			return err
		}
	}
	return nil
}

// --- Built-in enforcers ---

type allowedCommandsEnforcer struct{}

func (allowedCommandsEnforcer) Name() string { return "allowed_commands" }
func (allowedCommandsEnforcer) Check(_ context.Context, _, _ string, params json.RawMessage, value any) error {
	allowed := asStringSet(value)
	if len(allowed) == 0 {
		return nil
	}
	cmd := firstWord(extractParam(params, "command"))
	if cmd == "" {
		return nil
	}
	if _, ok := allowed[cmd]; !ok {
		return &ErrConstraintViolation{Constraint: "allowed_commands", Reason: fmt.Sprintf("%q not in allowlist", cmd)}
	}
	return nil
}

type blockedCommandsEnforcer struct{}

func (blockedCommandsEnforcer) Name() string { return "blocked_commands" }
func (blockedCommandsEnforcer) Check(_ context.Context, _, _ string, params json.RawMessage, value any) error {
	blocked := asStringSet(value)
	cmd := firstWord(extractParam(params, "command"))
	if cmd == "" {
		return nil
	}
	if _, ok := blocked[cmd]; ok {
		return &ErrConstraintViolation{Constraint: "blocked_commands", Reason: fmt.Sprintf("%q is blocklisted", cmd)}
	}
	return nil
}

type allowedPathsEnforcer struct{}

func (allowedPathsEnforcer) Name() string { return "allowed_paths" }
func (allowedPathsEnforcer) Check(_ context.Context, _, _ string, params json.RawMessage, value any) error {
	allow := asStringList(value)
	if len(allow) == 0 {
		return nil
	}
	path := extractParam(params, "path")
	if path == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	for _, base := range allow {
		bAbs, err := filepath.Abs(base)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(bAbs, abs)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return nil
		}
	}
	return &ErrConstraintViolation{Constraint: "allowed_paths", Reason: fmt.Sprintf("%q is outside allowed roots", path)}
}

type blockedActionsEnforcer struct{}

func (blockedActionsEnforcer) Name() string { return "blocked_actions" }
func (blockedActionsEnforcer) Check(_ context.Context, _, toolName string, _ json.RawMessage, value any) error {
	blocked := asStringSet(value)
	if _, ok := blocked[toolName]; ok {
		return &ErrConstraintViolation{Constraint: "blocked_actions", Reason: fmt.Sprintf("tool %q is blocked", toolName)}
	}
	return nil
}

type allowedActionsEnforcer struct{}

func (allowedActionsEnforcer) Name() string { return "allowed_actions" }
func (allowedActionsEnforcer) Check(_ context.Context, _, toolName string, _ json.RawMessage, value any) error {
	allowed := asStringSet(value)
	if len(allowed) == 0 {
		return nil
	}
	if _, ok := allowed[toolName]; !ok {
		return &ErrConstraintViolation{Constraint: "allowed_actions", Reason: fmt.Sprintf("tool %q is not in allowlist", toolName)}
	}
	return nil
}

func asStringSet(v any) map[string]struct{} {
	out := map[string]struct{}{}
	for _, s := range asStringList(v) {
		out[s] = struct{}{}
	}
	return out
}

func asStringList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, it := range t {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}

func extractParam(params json.RawMessage, key string) string {
	if len(params) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(params, &m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			return s[:i]
		}
	}
	return s
}
