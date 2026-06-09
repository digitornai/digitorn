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
	"errors"
	"fmt"
	"strings"
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
		Name:        "history",
		Description: "List the committed revisions of one file (approval history) — newest changes from the shadow repo's git log.",
		Internal:    true,
		RiskLevel:   tool.RiskLow,
		Params: []tool.ParamSpec{wd,
			{Name: "path", Type: "string", Description: "File path (forward slashes).", Required: true}},
		Handler: m.history,
	})
	m.RegisterTool(module.Tool{
		Name:        "commit",
		Description: "Commit the approved (staged) changes — advancing the baseline. Any paths given are staged first; unstaged pending files are left.",
		Internal:    true,
		RiskLevel:   tool.RiskMedium,
		Params: []tool.ParamSpec{wd,
			{Name: "message", Type: "string", Description: "Commit message."},
			{Name: "paths", Type: "array", Description: "Paths to stage then commit; empty = commit what's approved.", Items: &tool.ParamSpec{Type: "string"}}},
		Handler: m.commit,
	})
	m.RegisterTool(module.Tool{
		Name:        "approve",
		Description: "Approve paths — commit them in the shadow repo as one revision (approval = a committed revision, visible in history). Empty 'paths' approves the whole pending set. The optional message becomes the revision label.",
		Internal:    true,
		RiskLevel:   tool.RiskLow,
		Params: []tool.ParamSpec{wd,
			{Name: "paths", Type: "array", Description: "Paths to approve (commit); empty = approve all pending.", Items: &tool.ParamSpec{Type: "string"}},
			{Name: "message", Type: "string", Description: "Optional revision label; empty = an auto label."}},
		Handler: m.approve,
	})
	m.RegisterTool(module.Tool{
		Name:        "reject",
		Description: "Reject the given paths — restore each to the baseline (a modified file reverts, a newly-added file is removed).",
		Internal:    true,
		RiskLevel:   tool.RiskMedium,
		Params: []tool.ParamSpec{wd,
			{Name: "paths", Type: "array", Description: "Paths to reject (revert to baseline).", Items: &tool.ParamSpec{Type: "string"}}},
		Handler: m.reject,
	})
	m.RegisterTool(module.Tool{
		Name:        "revert",
		Description: "Revert one file to a past revision — write that revision's content to the working tree as a fresh pending change (history is untouched; the user then approves or rejects it).",
		Internal:    true,
		RiskLevel:   tool.RiskMedium,
		Params: []tool.ParamSpec{wd,
			{Name: "path", Type: "string", Description: "File path (forward slashes).", Required: true},
			{Name: "revision", Type: "integer", Description: "1-based revision to restore (oldest first), as listed by history.", Required: true}},
		Handler: m.revert,
	})
	m.RegisterTool(module.Tool{
		Name:        "log",
		Description: "List the whole workspace history — every approval (shadow-repo commit), newest first, with the files each one changed.",
		Internal:    true,
		RiskLevel:   tool.RiskLow,
		Params:      []tool.ParamSpec{wd},
		Handler:     m.log,
	})
	m.RegisterTool(module.Tool{
		Name:        "revert_commit",
		Description: "Restore files to their content at a past commit (all files the commit changed, or the chosen subset) as a pending change — 'go back to this point'. History is untouched.",
		Internal:    true,
		RiskLevel:   tool.RiskMedium,
		Params: []tool.ParamSpec{wd,
			{Name: "sha", Type: "string", Description: "Commit hash, as listed by log.", Required: true},
			{Name: "paths", Type: "array", Description: "Files to restore; empty = every file the commit changed.", Items: &tool.ParamSpec{Type: "string"}}},
		Handler: m.revertCommit,
	})
	m.RegisterTool(module.Tool{
		Name:        "approve_hunks",
		Description: "Approve specific hunks of one file — commit baseline + the selected hunks, leaving the rest pending. Hunks are identified by the stable hash from the file's diff.",
		Internal:    true,
		RiskLevel:   tool.RiskLow,
		Params: []tool.ParamSpec{wd,
			{Name: "path", Type: "string", Description: "File path (forward slashes).", Required: true},
			{Name: "hunks", Type: "array", Description: "Hunk hashes (from the diff) to approve.", Required: true, Items: &tool.ParamSpec{Type: "string"}},
			{Name: "message", Type: "string", Description: "Optional revision label."}},
		Handler: m.approveHunks,
	})
	m.RegisterTool(module.Tool{
		Name:        "reject_hunks",
		Description: "Reject specific hunks of one file — revert only those hunks in the working tree, keeping the rest. Hunks are identified by the stable hash from the file's diff.",
		Internal:    true,
		RiskLevel:   tool.RiskMedium,
		Params: []tool.ParamSpec{wd,
			{Name: "path", Type: "string", Description: "File path (forward slashes).", Required: true},
			{Name: "hunks", Type: "array", Description: "Hunk hashes (from the diff) to reject.", Required: true, Items: &tool.ParamSpec{Type: "string"}}},
		Handler: m.rejectHunks,
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

type stageParams struct {
	Workdir string   `json:"workdir"`
	Paths   []string `json:"paths"`
}

type approveParams struct {
	Workdir string   `json:"workdir"`
	Paths   []string `json:"paths"`
	Message string   `json:"message"`
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
	// Ensure the baseline exists before listing changes: without it every file
	// reads as untracked (the whole repo shows as "added"). The baseline is built
	// in one pass and persists on disk, so this is a fast no-op after the first
	// call. Best-effort — a baseline error still lets the raw status through.
	_, _ = r.EnsureBaseline()
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
		validation := "pending"
		if c.Staged {
			validation = "approved"
		}
		// The *_pending counters reflect changes still awaiting review. An
		// approved (staged) file has nothing pending, so zero its +/- — the
		// gutter badge clears on approve — while the diff body stays available
		// for the user to review what was approved.
		pendIns, pendDel := ins, del
		if c.Staged {
			pendIns, pendDel = 0, 0
		}
		totalIns += pendIns
		totalDel += pendDel
		files = append(files, map[string]any{
			"path":                 c.Path,
			"status":               c.Status,
			"validation":           validation,
			"insertions_pending":   pendIns,
			"deletions_pending":    pendDel,
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

func (m *Module) history(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var p diffParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Workdir == "" || p.Path == "" {
		return errResult("history: 'workdir' and 'path' are required"), nil
	}
	r, err := m.repo(p.Workdir)
	if err != nil {
		return errResult(err.Error()), nil
	}
	revs, err := r.History(p.Path)
	if err != nil {
		return errResult(err.Error()), nil
	}
	return tool.Result{Success: true, Data: map[string]any{"revisions": revs}}, nil
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

// approve COMMITS the approved set as one revision (approval = a committed
// revision, so it shows up in a file's history with its message as the label).
// Empty paths = "approve all": every pending change is staged, then committed in
// one shot. An empty message falls back to a sensible auto label so a revision is
// never unlabeled. Approving with nothing pending is a no-op, not an error.
func (m *Module) approve(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var p approveParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Workdir == "" {
		return errResult("approve: 'workdir' is required"), nil
	}
	r, err := m.repo(p.Workdir)
	if err != nil {
		return errResult(err.Error()), nil
	}
	all := len(p.Paths) == 0
	if all {
		if err := r.StageAll(); err != nil {
			return errResult(err.Error()), nil
		}
	}
	sha, err := r.Commit(approveMessage(p.Message, p.Paths), p.Paths)
	if errors.Is(err, gitrepo.ErrNothingStaged) {
		return tool.Result{Success: true, Data: map[string]any{"approved": 0}}, nil
	}
	if err != nil {
		return errResult(err.Error()), nil
	}
	approved := any(len(p.Paths))
	if all {
		approved = "all"
	}
	return tool.Result{Success: true, Data: map[string]any{"approved": approved, "sha": sha}}, nil
}

// approveMessage returns the revision label: the user's message when set, else a
// concise auto label from the approved paths.
func approveMessage(msg string, paths []string) string {
	if m := strings.TrimSpace(msg); m != "" {
		return m
	}
	switch len(paths) {
	case 0:
		return "Approve all changes"
	case 1:
		return "Approve " + paths[0]
	default:
		return fmt.Sprintf("Approve %d files", len(paths))
	}
}

type revertParams struct {
	Workdir  string `json:"workdir"`
	Path     string `json:"path"`
	Revision int    `json:"revision"`
}

// revert restores one file to a past revision as a pending change (the content
// is written to the working tree; the shadow-repo history is untouched).
func (m *Module) revert(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var p revertParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Workdir == "" || p.Path == "" {
		return errResult("revert: 'workdir' and 'path' are required"), nil
	}
	if p.Revision < 1 {
		return errResult("revert: 'revision' must be >= 1"), nil
	}
	r, err := m.repo(p.Workdir)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := r.RestoreRevision(p.Path, p.Revision); err != nil {
		return errResult(err.Error()), nil
	}
	return tool.Result{Success: true, Data: map[string]any{"reverted": p.Revision}}, nil
}

func (m *Module) log(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var p wdParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Workdir == "" {
		return errResult("log: 'workdir' is required"), nil
	}
	r, err := m.repo(p.Workdir)
	if err != nil {
		return errResult(err.Error()), nil
	}
	commits, err := r.Log()
	if err != nil {
		return errResult(err.Error()), nil
	}
	return tool.Result{Success: true, Data: map[string]any{"commits": commits}}, nil
}

type revertCommitParams struct {
	Workdir string   `json:"workdir"`
	Sha     string   `json:"sha"`
	Paths   []string `json:"paths"`
}

// revertCommit restores a commit's files (all or a chosen subset) to their
// content at that commit as a pending change. History is untouched.
func (m *Module) revertCommit(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var p revertCommitParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Workdir == "" || p.Sha == "" {
		return errResult("revert_commit: 'workdir' and 'sha' are required"), nil
	}
	r, err := m.repo(p.Workdir)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := r.RestoreCommit(p.Sha, p.Paths); err != nil {
		return errResult(err.Error()), nil
	}
	reverted := any(len(p.Paths))
	if len(p.Paths) == 0 {
		reverted = "all"
	}
	return tool.Result{Success: true, Data: map[string]any{"reverted": reverted, "sha": p.Sha}}, nil
}

func (m *Module) reject(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var p stageParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Workdir == "" {
		return errResult("reject: 'workdir' is required"), nil
	}
	r, err := m.repo(p.Workdir)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := r.Restore(p.Paths); err != nil {
		return errResult(err.Error()), nil
	}
	return tool.Result{Success: true, Data: map[string]any{"rejected": len(p.Paths)}}, nil
}

type hunksParams struct {
	Workdir string   `json:"workdir"`
	Path    string   `json:"path"`
	Hunks   []string `json:"hunks"`
	Message string   `json:"message"`
}

func (m *Module) approveHunks(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var p hunksParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Workdir == "" || p.Path == "" {
		return errResult("approve_hunks: 'workdir' and 'path' are required"), nil
	}
	if len(p.Hunks) == 0 {
		return errResult("approve_hunks: 'hunks' must list at least one hunk hash"), nil
	}
	r, err := m.repo(p.Workdir)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := r.ApproveHunks(p.Path, p.Hunks, p.Message); err != nil {
		return errResult(err.Error()), nil
	}
	return tool.Result{Success: true, Data: map[string]any{"approved_hunks": len(p.Hunks), "path": p.Path}}, nil
}

func (m *Module) rejectHunks(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var p hunksParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Workdir == "" || p.Path == "" {
		return errResult("reject_hunks: 'workdir' and 'path' are required"), nil
	}
	if len(p.Hunks) == 0 {
		return errResult("reject_hunks: 'hunks' must list at least one hunk hash"), nil
	}
	r, err := m.repo(p.Workdir)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := r.RejectHunks(p.Path, p.Hunks); err != nil {
		return errResult(err.Error()), nil
	}
	return tool.Result{Success: true, Data: map[string]any{"rejected_hunks": len(p.Hunks), "path": p.Path}}, nil
}

func errResult(msg string) tool.Result {
	return tool.Result{Success: false, Error: msg}
}
