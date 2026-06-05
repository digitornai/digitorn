// Package workspace exposes git-backed change tracking for a session
// workspace: list the agent's modified files, diff them, and commit ("validate")
// them. It is a thin module over internal/gitrepo (a per-workdir SHADOW repo
// under <workdir>/.digitorn/git that tracks ONLY the agent's changes and never
// touches a user's own .git).
//
// All actions are Internal (hidden from the LLM): they are driven by the
// daemon's workspace coordinator + REST routes, not by the model. The module
// is designed to run inside a worker pool, off the daemon hot path — every
// action takes the session workdir as an explicit param because the worker is
// a separate process sharing the filesystem.
package workspace

import (
	"context"
	"encoding/json"
	"sync"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/gitrepo"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// Module is the workspace git-tracking module.
type Module struct {
	module.Base

	mu    sync.Mutex
	repos map[string]*gitrepo.Repo // workdir -> shadow repo (cached, thread-safe)
}

// New constructs the workspace module with its internal git actions.
func New() *Module {
	m := &Module{repos: make(map[string]*gitrepo.Repo)}
	m.Base = module.Base{
		ID:          "workspace",
		Version:     "1.0.0",
		Description: "Git-backed change tracking for a session workspace (status / diff / commit).",
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux,
			domainmodule.PlatformMacOS,
			domainmodule.PlatformWindows,
		},
	}

	wd := tool.ParamSpec{Name: "workdir", Type: "string", Description: "Absolute session workdir.", Required: true}

	m.RegisterTool(module.Tool{
		Name:        "baseline",
		Description: "Initialise the shadow repo and commit the workspace's starting state (HEAD baseline) if not already done.",
		Internal:    true,
		RiskLevel:   tool.RiskLow,
		Params:      []tool.ParamSpec{wd},
		Handler:     m.baseline,
	})
	m.RegisterTool(module.Tool{
		Name:        "changes",
		Description: "List the agent's pending file changes since the baseline (added / modified / deleted).",
		Internal:    true,
		RiskLevel:   tool.RiskLow,
		Params:      []tool.ParamSpec{wd},
		Handler:     m.changes,
	})
	m.RegisterTool(module.Tool{
		Name:        "diff",
		Description: "Unified diff plus insertion/deletion counts for one changed file (HEAD vs working tree).",
		Internal:    true,
		RiskLevel:   tool.RiskLow,
		Params: []tool.ParamSpec{wd,
			{Name: "path", Type: "string", Description: "File path (forward slashes).", Required: true}},
		Handler: m.diff,
	})
	m.RegisterTool(module.Tool{
		Name:        "commit",
		Description: "Validate (commit) the given paths — or all pending changes when paths is empty — advancing the baseline.",
		Internal:    true,
		RiskLevel:   tool.RiskMedium,
		Params: []tool.ParamSpec{wd,
			{Name: "message", Type: "string", Description: "Commit message."},
			{Name: "paths", Type: "array", Description: "Paths to validate; empty = all pending.", Items: &tool.ParamSpec{Type: "string"}}},
		Handler: m.commit,
	})

	return m
}

// repo returns the cached shadow repo for a workdir, opening it on first use.
func (m *Module) repo(workdir string) (*gitrepo.Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.repos[workdir]; ok {
		return r, nil
	}
	r, err := gitrepo.Open(workdir)
	if err != nil {
		return nil, err
	}
	m.repos[workdir] = r
	return r, nil
}

type wdParams struct {
	Workdir string `json:"workdir"`
}

type changesParams struct {
	Workdir      string `json:"workdir"`
	IncludeDiffs bool   `json:"include_diffs"`
}

type diffParams struct {
	Workdir string `json:"workdir"`
	Path    string `json:"path"`
}

type commitParams struct {
	Workdir string   `json:"workdir"`
	Message string   `json:"message"`
	Paths   []string `json:"paths"`
}

func (m *Module) baseline(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var p wdParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Workdir == "" {
		return errResult("baseline: 'workdir' is required"), nil
	}
	r, err := m.repo(p.Workdir)
	if err != nil {
		return errResult(err.Error()), nil
	}
	created, err := r.EnsureBaseline()
	if err != nil {
		return errResult(err.Error()), nil
	}
	return tool.Result{Success: true, Data: map[string]any{"created": created}}, nil
}

func (m *Module) changes(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var p changesParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Workdir == "" {
		return errResult("changes: 'workdir' is required"), nil
	}
	r, err := m.repo(p.Workdir)
	if err != nil {
		return errResult(err.Error()), nil
	}
	ch, err := r.Changes()
	if err != nil {
		return errResult(err.Error()), nil
	}
	// Lean by default (the live-push poke + the daemon notifier only need the
	// list). include_diffs enriches each file with its unified diff + counts so
	// the web Changes panel renders without a per-file round-trip.
	if !p.IncludeDiffs {
		return tool.Result{Success: true, Data: map[string]any{"files": ch}}, nil
	}
	files := make([]map[string]any, 0, len(ch))
	totalIns, totalDel := 0, 0
	for _, c := range ch {
		unified, ins, del, derr := r.FileDiff(c.Path)
		if derr != nil {
			unified = ""
		}
		totalIns += ins
		totalDel += del
		files = append(files, map[string]any{
			"path":                 c.Path,
			"status":               c.Status,
			"insertions_pending":   ins,
			"deletions_pending":    del,
			"unified_diff_pending": unified,
		})
	}
	return tool.Result{Success: true, Data: map[string]any{
		"files":                    files,
		"count":                    len(files),
		"total_insertions_pending": totalIns,
		"total_deletions_pending":  totalDel,
	}}, nil
}

func (m *Module) diff(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var p diffParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Workdir == "" || p.Path == "" {
		return errResult("diff: 'workdir' and 'path' are required"), nil
	}
	r, err := m.repo(p.Workdir)
	if err != nil {
		return errResult(err.Error()), nil
	}
	unified, ins, del, err := r.FileDiff(p.Path)
	if err != nil {
		return errResult(err.Error()), nil
	}
	return tool.Result{Success: true, Data: map[string]any{
		"path": p.Path, "unified": unified, "insertions": ins, "deletions": del,
	}}, nil
}

func (m *Module) commit(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var p commitParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Workdir == "" {
		return errResult("commit: 'workdir' is required"), nil
	}
	r, err := m.repo(p.Workdir)
	if err != nil {
		return errResult(err.Error()), nil
	}
	sha, err := r.Commit(p.Message, p.Paths)
	if err != nil {
		return errResult(err.Error()), nil
	}
	return tool.Result{Success: true, Data: map[string]any{"sha": sha}}, nil
}

func errResult(msg string) tool.Result {
	return tool.Result{Success: false, Error: msg}
}
