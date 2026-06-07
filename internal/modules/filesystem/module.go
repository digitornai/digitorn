// Package filesystem exposes read/write/edit/ls/grep/glob actions over a
// path-restricted workspace. Every path is normalized and validated to stay
// inside the configured workspace root — symlinks pointing outside are rejected.
package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/workdir"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// Config is the per-app configuration for the filesystem module.
type Config struct {
	Workspace    string `json:"workspace" yaml:"workspace"`           // root directory; defaults to CWD
	MaxFileBytes int64  `json:"max_file_bytes" yaml:"max_file_bytes"` // 0 = no limit
}

// Module is the filesystem module instance.
type Module struct {
	module.Base
	cfg Config
}

// PromptSections contributes the filesystem module's operating guidance to the
// system prompt of every agent AUTHORIZED for it (the framework gathers this
// automatically — no per-call wiring). Implements domainmodule.PromptContributor.
func (m *Module) PromptSections(domainmodule.PromptScope) []domainmodule.PromptSection {
	return []domainmodule.PromptSection{{
		Title:    "Filesystem",
		Priority: 50,
		Content: "All paths are relative to the workspace root; absolute paths and `..` escapes are rejected.\n" +
			"Workflow that always works:\n" +
			"1. FIND: `glob` by name (`**` recurses) or `grep` contents — never guess a path.\n" +
			"2. MAP a big file: `read` with `outline: true` → functions/classes + line numbers, cheaply; then `read` a precise line range (offset/limit). Read several files at once with `paths: [...]`. Reading an IMAGE (png/jpg/…) shows it to you directly — you can SEE it.\n" +
			"3. EDIT surgically — pick the easiest locator, you do NOT need to retype the file:\n" +
			"   • by LINE (best): `edit{start_line, end_line, new_string}` to replace those lines (you saw the numbers in read); empty new_string deletes them.\n" +
			"   • INSERT: `edit{insert_after|insert_before: \"<unique line snippet>\", new_string}`, or `prepend`/`append`.\n" +
			"   • by TEXT: `edit{old_string, new_string}`; if it occurs many times add context, or set `occurrence: N`, or `replace_all`.\n" +
			"   Set `dry_run: true` first to preview the diff on a risky edit; set `expect` to a snippet the target must contain so you never edit the wrong place. `multi_edit` applies several edits to one file atomically.\n" +
			"`write` is only for creating or fully replacing a file. After an edit, the result shows a diff (+N/−N) confirming exactly what changed.",
	}}
}

// DynamicToolPrompts contributes no runtime tool-prompt overlays (the
// filesystem tools' guidance is static, declared on their specs).
func (m *Module) DynamicToolPrompts(domainmodule.PromptScope) map[string]string { return nil }

// New constructs the filesystem module with all its actions wired.
func New() *Module {
	m := &Module{}
	m.Base = module.Base{
		ID:          "filesystem",
		Version:     "1.0.0",
		Description: "Read, write, edit, list and search files under a workspace.",
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux,
			domainmodule.PlatformMacOS,
			domainmodule.PlatformWindows,
		},
	}

	m.RegisterTool(module.Tool{
		Name:        "read",
		Description: "Read a file with line numbers (cat -n style). IMAGES (PNG/JPG/GIF/WEBP/BMP) are returned as actual visual content you can SEE, not described. Use `outline: true` on a big code file to get a structural map (functions/classes/headings + line numbers) instead of the full content — then read a precise line range or edit by line number. Pass `paths` (a list) to read several files (and/or images) in one call.",
		ToolPrompt: "Read before you edit or write — never edit a file blind. When you already know the symbol or region you need, read just that range (offset/limit) rather than the whole file; on a large or unfamiliar file run `outline: true` first to map it, then read the precise lines.\n" +
			"Batch related files in ONE call via `paths` instead of many sequential reads — it is faster and keeps the picture coherent.\n" +
			"The line numbers in the output are authoritative: cite locations as path:line and edit by those numbers.\n" +
			"Do NOT re-read a file you just wrote or edited to confirm it worked — the write/edit tool already errors on failure, and the harness tracks the current content for you. Reading to verify is wasted effort.",
		Params: []tool.ParamSpec{
			{Name: "path", Type: "string", Description: "File path relative to the workspace.", Path: true},
			{Name: "paths", Type: "array", Description: "Read several files in one call (labeled sections). Use instead of path.", Items: &tool.ParamSpec{Type: "string", Path: true}},
			{Name: "offset", Type: "integer", Description: "1-based line to start from (default 1).", Default: 1},
			{Name: "limit", Type: "integer", Description: "Max lines to return (default 2000).", Default: 0},
			{Name: "outline", Type: "boolean", Description: "Return a structural map (definitions + line numbers) instead of content — ideal to navigate a large file cheaply.", Default: false},
		},
		RiskLevel: tool.RiskLow,
		Handler:   m.read,
	})

	m.RegisterTool(module.Tool{
		Name:        "write",
		Description: "Write content to a file, creating it if it does not exist (overwrites).",
		ToolPrompt: "Use `write` for a NEW file or a deliberate full rewrite — it replaces the entire file. To change part of an existing file use `edit`/`multi_edit` instead: surgical edits preserve the rest and are far less likely to drop code than re-emitting the whole file from memory.\n" +
			"If the file already exists, read it first so you don't clobber content you didn't mean to.\n" +
			"Match the surrounding code's style and conventions. Never write credentials, API keys, or secrets into source.",
		Params: []tool.ParamSpec{
			{Name: "path", Type: "string", Description: "File path relative to the workspace.", Required: true, Path: true},
			{Name: "content", Type: "string", Description: "Full content to write.", Required: true},
		},
		RiskLevel:    tool.RiskMedium,
		Irreversible: true,
		Handler:      m.write,
	})

	m.RegisterTool(module.Tool{
		Name: "edit",
		Description: "Edit a file surgically. Pick ONE way to locate the edit, then `new_string` is the content to put there:\n" +
			"• By LINE NUMBER (easiest — you saw the numbers in `read`): set `start_line` (and `end_line` for a range) to replace those lines. `new_string` empty deletes them. No need to reproduce the text.\n" +
			"• INSERT: `insert_after` / `insert_before` a short unique snippet from the target line, or `prepend` / `append` to add at the file's start/end.\n" +
			"• By TEXT: `old_string` (exact match, with a forgiving whitespace/indentation fallback). If it occurs N times: add surrounding context, OR set `occurrence` to the Nth match, OR `replace_all`.\n" +
			"Set `dry_run` to preview the unified diff without writing. `expect` (optional) is a snippet the target must still contain — if not, the edit is refused (guards against editing the wrong place after the file changed).",
		ToolPrompt: "Always `read` the target first — the edit is anchored to text/lines you must have seen. Right after a read, line locators (start_line/end_line) are the cheapest and most unambiguous; switch to text/anchor locators (old_string, insert_after/before) once earlier edits may have shifted the line numbers.\n" +
			"`old_string` must uniquely identify the spot: if it matches more than once, add surrounding context, or set `occurrence` to the Nth match, or `replace_all` when you truly mean every one.\n" +
			"When unsure, set `dry_run: true` and inspect the diff before committing. Use `expect` on a risky edit so it refuses if the file isn't what you think.\n" +
			"Preserve surrounding indentation and style exactly; an edit that breaks indentation is a bug.",
		Params: []tool.ParamSpec{
			{Name: "path", Type: "string", Description: "File path relative to the workspace.", Required: true, Path: true},
			{Name: "new_string", Type: "string", Description: "Content to insert or to replace the located region with. Empty string deletes the targeted lines."},
			{Name: "old_string", Type: "string", Description: "TEXT locator: substring to find (exact, with whitespace/indentation fuzzy fallback)."},
			{Name: "replace_all", Type: "boolean", Description: "With old_string: replace every occurrence.", Default: false},
			{Name: "occurrence", Type: "integer", Description: "With old_string: replace ONLY the Nth match (1-based)."},
			{Name: "start_line", Type: "integer", Description: "LINE locator: first line to replace (1-based, as shown by read)."},
			{Name: "end_line", Type: "integer", Description: "LINE locator: last line to replace (1-based, inclusive). Omit for a single line."},
			{Name: "insert_after", Type: "string", Description: "INSERT new_string after the unique line containing this text."},
			{Name: "insert_before", Type: "string", Description: "INSERT new_string before the unique line containing this text."},
			{Name: "prepend", Type: "boolean", Description: "Insert new_string at the very start of the file.", Default: false},
			{Name: "append", Type: "boolean", Description: "Append new_string at the end of the file.", Default: false},
			{Name: "expect", Type: "string", Description: "Safety check: the targeted region must contain this text or the edit is refused."},
			{Name: "dry_run", Type: "boolean", Description: "Preview the unified diff without writing anything.", Default: false},
		},
		RiskLevel:    tool.RiskMedium,
		Irreversible: true,
		Handler:      m.edit,
	})

	m.RegisterTool(module.Tool{
		Name:        "multi_edit",
		Description: "Apply several edits to one file in a single atomic write (all-or-nothing). Edits apply in order; each sees the previous result. Each edit accepts the SAME locators as `edit` (old_string / occurrence / insert_after / insert_before / prepend / append; expect). Prefer text/anchor locators here — line numbers shift as earlier edits apply. Set dry_run to preview the combined diff.",
		ToolPrompt:  "Prefer this over several separate `edit` calls when changing one file in multiple places — it's atomic (all edits land or none do) and you review one combined diff. Because edits apply in sequence, use text/anchor locators, not line numbers (earlier edits move later lines). Order edits top-to-bottom and make each `old_string` unique. `dry_run: true` to preview the whole change first.",
		Params: []tool.ParamSpec{
			{Name: "path", Type: "string", Description: "File path relative to the workspace.", Required: true, Path: true},
			{Name: "dry_run", Type: "boolean", Description: "Preview the combined unified diff without writing.", Default: false},
			{Name: "edits", Type: "array", Description: "Edits applied in order.", Required: true, Items: &tool.ParamSpec{
				Type: "object",
				Properties: []tool.ParamSpec{
					{Name: "old_string", Type: "string", Description: "Text locator: substring to find (exact + fuzzy fallback)."},
					{Name: "new_string", Type: "string", Description: "Content to insert/replace. Empty deletes the located region."},
					{Name: "replace_all", Type: "boolean", Description: "Replace every occurrence.", Default: false},
					{Name: "occurrence", Type: "integer", Description: "Replace only the Nth match (1-based)."},
					{Name: "insert_after", Type: "string", Description: "Insert new_string after the unique line containing this."},
					{Name: "insert_before", Type: "string", Description: "Insert new_string before the unique line containing this."},
					{Name: "expect", Type: "string", Description: "Safety check: located region must contain this."},
				},
			}},
		},
		RiskLevel:    tool.RiskMedium,
		Irreversible: true,
		Handler:      m.multiEdit,
	})

	m.RegisterTool(module.Tool{
		Name:        "delete",
		Description: "Delete a single file from the workspace (irreversible). Errors if the path does not exist or is a directory.",
		ToolPrompt: "Remove a file you created or no longer need. This is irreversible — the file is gone from disk and disappears from the client's view. It deletes ONE file, never a directory, and errors if the path is missing so a delete never silently no-ops.\n" +
			"Prefer editing over delete-then-rewrite; reach for delete only when the file should genuinely cease to exist.",
		Params: []tool.ParamSpec{
			{Name: "path", Type: "string", Description: "File path relative to the workspace.", Required: true, Path: true},
		},
		RiskLevel:    tool.RiskMedium,
		Irreversible: true,
		Handler:      m.delete,
	})

	m.RegisterTool(module.Tool{
		Name:        "glob",
		Description: "Find paths matching a glob pattern (supports ** for recursion), newest first. VCS/build dirs are skipped.",
		ToolPrompt:  "Reach for `glob` when you know the NAME or path shape (\"**/*.go\", \"src/**/*.{ts,tsx}\", \"cmd/*/main.go\"); reach for `grep` when you know the CONTENT. Full glob syntax is supported (recursive **, brace alternation {a,b}, ranges, character classes). Results are newest-first, so it doubles as \"what changed recently\". Use it to discover where things live before reading — don't guess paths.",
		Params: []tool.ParamSpec{
			{Name: "pattern", Type: "string", Description: "Glob pattern, e.g. \"**/*.go\" or \"src/*.ts\".", Required: true},
			{Name: "type", Type: "string", Description: "Filter: \"file\", \"dir\", or \"any\" (default).", Default: "any"},
			{Name: "max_results", Type: "integer", Description: "Cap on matches (default 1000).", Default: 0},
		},
		RiskLevel: tool.RiskLow,
		Handler:   m.glob,
	})

	m.RegisterTool(module.Tool{
		Name:        "grep",
		Description: "Search file contents matching a regular expression.",
		ToolPrompt: "Your primary way to locate code by content — searching is almost always cheaper and more accurate than reading whole files or directories. Scope it: set `include` to a file glob (\"*.go\", \"*.{ts,tsx}\") and `path` to the relevant subtree so results stay sharp. Search for a distinctive token (a function name, error string, struct field), read the few hits, then act.\n" +
			"For a broad, open-ended sweep across many angles or a large codebase, delegate to the explore sub-agent instead of running many greps yourself — it returns the conclusion without flooding your context.",
		Params: []tool.ParamSpec{
			{Name: "pattern", Type: "string", Description: "Regular expression to search for.", Required: true},
			{Name: "path", Type: "string", Description: "Directory to search under (default: workspace root).", Default: ".", Path: true},
			{Name: "include", Type: "string", Description: "Optional glob to filter files, e.g. \"*.go\".", Default: ""},
			{Name: "max_results", Type: "integer", Description: "Cap on match count.", Default: 500},
		},
		RiskLevel: tool.RiskLow,
		Handler:   m.grep,
	})

	return m
}

// Init parses the module configuration.
func (m *Module) Init(ctx context.Context, cfg map[string]any) error {
	if cfg != nil {
		raw, _ := json.Marshal(cfg)
		_ = json.Unmarshal(raw, &m.cfg)
	}
	if m.cfg.Workspace == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("filesystem: getwd: %w", err)
		}
		m.cfg.Workspace = wd
	}
	abs, err := filepath.Abs(m.cfg.Workspace)
	if err != nil {
		return fmt.Errorf("filesystem: abs(%s): %w", m.cfg.Workspace, err)
	}
	// Canonicalise the workspace root (resolve symlinks) so the
	// containment check in resolve() compares like-for-like — otherwise a
	// symlinked workspace would make every real path look like an escape.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	m.cfg.Workspace = abs
	return nil
}

// resolve validates that target stays inside the workspace and returns the
// absolute path. It is hardened against two escapes the naive version missed :
//
//   - Absolute targets are rejected outright (a workspace path must be
//     relative ; an absolute path is never inside by construction here).
//   - Symlinks are resolved on the deepest EXISTING ancestor — not just when
//     the full target exists — then the not-yet-existing tail is re-appended.
//     This catches a planted symlink in a parent dir (e.g. workspace/evil ->
//     /etc, then a write to evil/passwd) which the exists-only EvalSymlinks
//     let through for create operations.
func (m *Module) resolve(target string) (string, error) {
	if target == "" {
		return "", errors.New("path must not be empty")
	}
	var abs string
	if filepath.IsAbs(target) {
		// An absolute path is accepted only if it resolves inside the
		// workspace — the containment check below rejects any escape. Agents
		// routinely emit absolute paths once they know the workdir root, so
		// rejecting them outright only makes the model fight the tool.
		abs = filepath.Clean(target)
	} else {
		a, err := filepath.Abs(filepath.Join(m.cfg.Workspace, target))
		if err != nil {
			return "", err
		}
		abs = a
	}
	resolved := resolveExistingPrefix(abs)
	rel, err := filepath.Rel(m.cfg.Workspace, resolved)
	if err != nil {
		return "", err
	}
	// rel == "." is the workspace itself ; any rel that is ".." or begins
	// with "../" escapes. The exact-and-separator check avoids matching a
	// legitimate sibling like "..foo".
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace %q", target, m.cfg.Workspace)
	}
	return resolved, nil
}

// resolveExistingPrefix resolves symlinks on the deepest existing ancestor
// of abs, then re-joins the remaining (non-existent) tail. So a symlink in a
// parent directory is followed for containment checking even when the final
// component doesn't exist yet (the create-then-escape case).
func resolveExistingPrefix(abs string) string {
	cur := abs
	tail := ""
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			if tail == "" {
				return real
			}
			return filepath.Join(real, tail)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs // reached the volume root with nothing resolvable
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// resolveCtx confines target using the per-call workdir PathPolicy when one
// rides on the dispatch context (the agent path : the chokepoint has already
// rewritten the arg to an enforced absolute path, and Enforce re-validates it —
// defense in depth). Without a policy (setup steps / CLI / admin) it falls back
// to the module's static workspace-rooted resolution, which keeps its strict
// "relative only" stance for non-agent callers.
func (m *Module) resolveCtx(ctx context.Context, target string) (string, error) {
	if pp, ok := workdir.PathPolicyFromContext(ctx); ok {
		return pp.Enforce(target)
	}
	return m.resolve(target)
}

// globBase returns the root that glob/grep results are listed relative to —
// the session workdir when a policy is present, else the static workspace.
// ok is false when there is a policy but no workdir (file ops are confined to
// nothing → callers return no results rather than leaking the CWD).
func (m *Module) globBase(ctx context.Context) (string, bool) {
	if pp, ok := workdir.PathPolicyFromContext(ctx); ok {
		if !pp.HasWorkdir() {
			return "", false
		}
		return pp.Root(), true
	}
	return m.cfg.Workspace, m.cfg.Workspace != ""
}

// relInside returns the slash-form path of abs under base, and false when abs
// escapes base. Used to confine glob results to the workdir even for patterns
// like "../*" (which filepath.Glob would otherwise resolve outside).
func relInside(base, abs string) (string, bool) {
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// --- Action handlers ---

type readParams struct {
	Path     string   `json:"path"`
	FilePath string   `json:"file_path"` // alias (Claude editor convention)
	Filename string   `json:"filename"`  // alias
	File     string   `json:"file"`      // alias
	Paths    []string `json:"paths"`     // read several files in one call (labeled sections)
	Offset   flexInt  `json:"offset"`    // 1-based start line (default 1)
	Limit    flexInt  `json:"limit"`     // max lines to return (default 2000)
	Outline  flexBool `json:"outline"`   // return a structural map (defs + line numbers) not content
}

const readDefaultLines = 2000

func (m *Module) read(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p readParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	p.Path = effectivePath(p.Path, p.FilePath, p.Filename, p.File)
	targets := p.Paths
	if len(targets) == 0 {
		if p.Path == "" {
			err := errors.New("read: provide 'path' (or 'paths' for several files)")
			return errResult(err), err
		}
		targets = []string{p.Path}
	}

	// Single file : Data is the text body ; an image/binary file also rides as an
	// OutputPart so the model actually SEES it (vision) via the multipart adapter.
	if len(targets) == 1 {
		body, blob, err := m.readBody(ctx, targets[0], p)
		if err != nil {
			return errResult(err), err
		}
		res := tool.Result{Success: true, Data: body, Display: &tool.DisplayHint{Type: "code", Title: filepath.Base(targets[0])}}
		if blob != nil {
			res.OutputParts = []tool.OutputPart{{Kind: tool.OutputText, Text: body}, *blob}
		}
		return res, nil
	}

	// Multi-file : labeled sections in ONE call. One unreadable file is reported
	// inline and never aborts the others. Any images are collected as OutputParts
	// so the model sees them all alongside the combined text.
	var b strings.Builder
	var blobs []tool.OutputPart
	for _, rel := range targets {
		fmt.Fprintf(&b, "===== %s =====\n", rel)
		body, blob, err := m.readBody(ctx, rel, p)
		if err != nil {
			fmt.Fprintf(&b, "[error: %v]\n\n", err)
			continue
		}
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
		if blob != nil {
			blobs = append(blobs, *blob)
		}
	}
	res := tool.Result{Success: true, Data: b.String(), Display: &tool.DisplayHint{Type: "code", Title: fmt.Sprintf("%d files", len(targets))}}
	if len(blobs) > 0 {
		res.OutputParts = append([]tool.OutputPart{{Kind: tool.OutputText, Text: b.String()}}, blobs...)
	}
	return res, nil
}

// maxVisionBytes caps the image size shipped inline to the model. Vision models
// reject very large images (and base64 inflates ~33%), so a bigger file is
// reported as text with a "resize first" hint instead of being silently dropped.
const maxVisionBytes = 5 << 20

// readBody resolves one file and returns its renderable body : a numbered slice
// (offset/limit), an outline (when p.Outline), or a descriptor for a non-text
// file. Pure per-file logic, shared by single- and multi-file read.
func (m *Module) readBody(ctx context.Context, rel string, p readParams) (string, *tool.OutputPart, error) {
	abs, err := m.resolveCtx(ctx, rel)
	if err != nil {
		return "", nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, fmt.Errorf("%s: no such file under the workspace — run glob to find the exact path first", rel)
		}
		return "", nil, err
	}
	if fi.IsDir() {
		return "", nil, fmt.Errorf("%s is a directory — use glob to list its entries", rel)
	}

	capBytes := m.cfg.MaxFileBytes
	if capBytes <= 0 {
		capBytes = 10 << 20
	}
	data, overCap, err := readCapped(abs, capBytes)
	if err != nil {
		return "", nil, err
	}
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	if k := detectKind(head); k.kind != "text" {
		switch k.kind {
		case "image":
			// Ship the image bytes as a vision part so the model SEES it (like a
			// human opening the file). Too-large images are reported as text.
			if overCap || int64(len(data)) > maxVisionBytes {
				return fmt.Sprintf("[image %s is %d bytes — too large to view inline (max %d); resize it first]", filepath.Base(abs), fi.Size(), maxVisionBytes), nil, nil
			}
			note := fmt.Sprintf("[image %s — %s, %d bytes (shown below)]", filepath.Base(abs), k.media, fi.Size())
			return note, &tool.OutputPart{Kind: tool.OutputImage, Bytes: data, Mime: k.media, Name: filepath.Base(abs)}, nil
		case "pdf":
			return fmt.Sprintf("[PDF: %s, %d bytes — binary document, text extraction not wired yet]", filepath.Base(abs), fi.Size()), nil, nil
		default:
			return fmt.Sprintf("[binary file: %s, %d bytes — not displayed]", filepath.Base(abs), fi.Size()), nil, nil
		}
	}

	// Outline mode : a structural map (definitions + line numbers) so a big file
	// is navigable in a few dozen lines. Falls back to a normal read when the
	// file has no recognizable structure (plain text, data).
	if p.Outline {
		if om, n := outlineOf(string(data)); n > 0 {
			return fmt.Sprintf("outline of %s (%d definitions):\n%s\nRead a line range (start_line/end_line via edit, or offset/limit here) for full detail.", filepath.Base(abs), n, om), nil, nil
		}
		// fall through to a normal read
	}

	lines := splitLines(string(data))
	total := len(lines)
	start := int(p.Offset) - 1
	if p.Offset <= 0 {
		start = 0
	}
	limit := int(p.Limit)
	if limit <= 0 {
		limit = readDefaultLines
	}
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}

	body := numberedSlice(lines, start, end)
	truncated := overCap || end < total || start > 0
	if truncated {
		note := fmt.Sprintf("\n[showing lines %d-%d of %d", start+1, end, total)
		if overCap {
			note += fmt.Sprintf("; file exceeds the %d-byte read cap and was clipped", capBytes)
		}
		if end < total {
			note += fmt.Sprintf("; pass offset=%d to continue", end+1)
		}
		body += note + "]"
	}
	return body, nil, nil
}

// readCapped reads up to cap bytes ; overCap reports the file was larger and got
// clipped. Uses a full read (not a single Read syscall, which may short-read).
func readCapped(abs string, cap int64) (data []byte, overCap bool, err error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	buf := make([]byte, cap+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, false, err
	}
	if int64(n) > cap {
		return buf[:cap], true, nil
	}
	return buf[:n], false, nil
}

type writeParams struct {
	Path     string `json:"path"`
	FilePath string `json:"file_path"`
	Filename string `json:"filename"`
	File     string `json:"file"`
	Content  string `json:"content"`
}

func (m *Module) write(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p writeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	p.Path = effectivePath(p.Path, p.FilePath, p.Filename, p.File)
	abs, err := m.resolveCtx(ctx, p.Path)
	if err != nil {
		return errResult(err), err
	}
	existed := false
	var oldContent string
	if fi, e := os.Stat(abs); e == nil {
		if fi.IsDir() {
			err := fmt.Errorf("%s is a directory — cannot overwrite with a file", p.Path)
			return errResult(err), err
		}
		existed = true
		// Read the old content for the diff only when it is small enough to be
		// worth carrying (the diff layer caps it too — this avoids slurping a
		// multi-MB file into memory just to throw it away).
		if fi.Size() <= diffContentCap {
			if b, e := os.ReadFile(abs); e == nil {
				oldContent = string(b)
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return errResult(err), err
	}
	// Atomic : a crash/power-loss leaves either the old file or the new one,
	// never a truncated one. Preserves the existing file's permissions.
	if err := atomicWrite(abs, []byte(p.Content), fileMode(abs, 0o644)); err != nil {
		return errResult(err), err
	}
	tindexes.markDirty(abs) // keep this file fresh against any trigram index
	sindexes.markDirty(abs) // and against the ephemeral semantic index
	notifyFileChange(ctx)   // live workspace push (non-blocking, best-effort)
	action := "created"
	if existed {
		action = "overwrote"
	}
	return tool.Result{
		Success: true,
		Data:    map[string]any{"path": p.Path, "bytes": len(p.Content), "action": action},
		Diff:    diffView(p.Path, oldContent, p.Content),
	}, nil
}

type editParams struct {
	Path string `json:"path"`
	// Aliases: weaker models key the file under file_path (Claude's editor
	// convention), filename, or file. Accept them so a correct edit is not
	// rejected with "path must not be empty" over the key name alone — the same
	// forgiveness the glob/grep tools already give the pattern argument.
	FilePath   string   `json:"file_path"`
	Filename   string   `json:"filename"`
	File       string   `json:"file"`
	OldString  string   `json:"old_string"`
	NewString  string   `json:"new_string"`
	ReplaceAll flexBool `json:"replace_all"`
	// Surgical locators (alternatives to old_string — provide exactly one).
	Occurrence   flexInt  `json:"occurrence"`
	StartLine    flexInt  `json:"start_line"`
	EndLine      flexInt  `json:"end_line"`
	InsertAfter  string   `json:"insert_after"`
	InsertBefore string   `json:"insert_before"`
	Prepend      flexBool `json:"prepend"`
	Append       flexBool `json:"append"`
	Expect       string   `json:"expect"`
	DryRun       flexBool `json:"dry_run"`
}

func (p editParams) locator() editLocator {
	return editLocator{
		OldString: p.OldString, NewString: p.NewString, ReplaceAll: bool(p.ReplaceAll),
		Occurrence: int(p.Occurrence), StartLine: int(p.StartLine), EndLine: int(p.EndLine),
		InsertAfter: p.InsertAfter, InsertBefore: p.InsertBefore,
		Prepend: bool(p.Prepend), Append: bool(p.Append), Expect: p.Expect,
	}
}

// editError distinguishes the two recoverable edit failures so callers can give
// the agent the right correction hint : a total miss carries the closest blocks,
// an ambiguous match carries the line numbers it hit.
type editError struct {
	kind    string // "empty" | "not_found" | "ambiguous"
	message string
	closest []suggestion
}

func (e *editError) Error() string { return e.message }

// applyEdit performs ONE old→new substitution against in-memory content and
// returns the result, without touching disk. Exact (byte-identical) match wins ;
// on an exact miss it falls back to the deterministic fuzzy locate (line-endings
// / trailing-space / indentation) — never a risky similarity guess. Shared by
// edit (single) and multi_edit (batch), so their matching semantics are identical.
func applyEdit(content, oldStr, newStr string, replaceAll bool) (updated string, count int, strategy string, err error) {
	if oldStr == "" {
		return "", 0, "", &editError{kind: "empty", message: "old_string must not be empty"}
	}
	if c := strings.Count(content, oldStr); c >= 1 {
		if c > 1 && !replaceAll {
			// Report WHERE the matches are (like the fuzzy path) so the agent can
			// add surrounding context to target ONE precisely, instead of blindly
			// using replace_all (which would change them all — often not intended).
			lines := make([]int, 0, c)
			for off := 0; len(lines) < 20; {
				i := strings.Index(content[off:], oldStr)
				if i < 0 {
					break
				}
				abs := off + i
				lines = append(lines, 1+strings.Count(content[:abs], "\n"))
				off = abs + len(oldStr)
			}
			return "", 0, "", &editError{kind: "ambiguous", message: fmt.Sprintf("old_string is not unique (%d matches at lines %v) — add surrounding context to target one, or pass replace_all=true to change them all", c, lines)}
		}
		if replaceAll {
			updated = strings.ReplaceAll(content, oldStr, newStr)
		} else {
			updated = strings.Replace(content, oldStr, newStr, 1)
		}
		return updated, c, "exact", nil
	}
	spans, strat := locateFuzzy(content, oldStr)
	if len(spans) == 0 {
		return "", 0, "", &editError{kind: "not_found", message: "old_string not found", closest: closestMatches(content, oldStr, 3)}
	}
	if len(spans) > 1 && !replaceAll {
		lines := make([]int, 0, len(spans))
		for _, s := range spans {
			lines = append(lines, 1+strings.Count(content[:s.start], "\n"))
		}
		return "", 0, "", &editError{kind: "ambiguous", message: fmt.Sprintf("old_string matches %d places (fuzzy, via %s) at lines %v — pass replace_all=true or add surrounding context", len(spans), strat, lines)}
	}
	return applyFuzzySpans(content, spans, newStr), len(spans), strat, nil
}

// effectivePath returns the first non-empty of the canonical path and the
// aliases a confused model reaches for (file_path / filename / file), so an edit
// keyed under the wrong name still lands instead of failing on an empty path.
func effectivePath(primary string, aliases ...string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	for _, a := range aliases {
		if strings.TrimSpace(a) != "" {
			return a
		}
	}
	return primary
}

func (m *Module) edit(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p editParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	p.Path = effectivePath(p.Path, p.FilePath, p.Filename, p.File)
	abs, err := m.resolveCtx(ctx, p.Path)
	if err != nil {
		return errResult(err), err
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return errResult(err), err
	}
	content := string(src)

	updated, count, strategy, err := resolveEditOp(content, p.locator())
	if err != nil {
		if ee, ok := err.(*editError); ok && ee.kind == "not_found" {
			data := map[string]any{"error": ee.message, "path": p.Path}
			if len(ee.closest) > 0 {
				data["closest_matches"] = ee.closest
			}
			return tool.Result{Success: false, Error: ee.message, Data: data}, err
		}
		return errResult(err), err
	}
	d := computeDiff(p.Path, content, updated)
	data := map[string]any{
		"path": p.Path, "replacements": count, "strategy": strategy,
		"fuzzy": isFuzzyStrategy(strategy), "additions": d.Added, "deletions": d.Removed,
	}
	if d.Summary != "" {
		data["summary"] = d.Summary
	}
	// dry_run : show the agent the unified diff and write NOTHING. Lets a weak
	// agent preview a surgical edit before committing it.
	if p.DryRun {
		data["dry_run"] = true
		data["note"] = "DRY RUN — nothing written. Re-call without dry_run to apply."
		if d.Unified != "" {
			data["diff"] = d.Unified
		}
		return tool.Result{Success: true, Data: data, Diff: diffView(p.Path, content, updated)}, nil
	}
	// Atomic write : a crash leaves the old or new file, never a truncated one ;
	// existing permissions preserved.
	if err := atomicWrite(abs, []byte(updated), fileMode(abs, 0o644)); err != nil {
		return errResult(err), err
	}
	tindexes.markDirty(abs)
	notifyFileChange(ctx) // live workspace push (non-blocking, best-effort)
	return tool.Result{Success: true, Data: data, Diff: diffView(p.Path, content, updated)}, nil
}

type multiEditParams struct {
	Path     string `json:"path"`
	FilePath string `json:"file_path"`
	Filename string `json:"filename"`
	File     string `json:"file"`
	Edits    []struct {
		OldString    string   `json:"old_string"`
		NewString    string   `json:"new_string"`
		ReplaceAll   flexBool `json:"replace_all"`
		Occurrence   flexInt  `json:"occurrence"`
		StartLine    flexInt  `json:"start_line"`
		EndLine      flexInt  `json:"end_line"`
		InsertAfter  string   `json:"insert_after"`
		InsertBefore string   `json:"insert_before"`
		Prepend      flexBool `json:"prepend"`
		Append       flexBool `json:"append"`
		Expect       string   `json:"expect"`
	} `json:"edits"`
	DryRun flexBool `json:"dry_run"`
}

// multiEdit applies a batch of edits to one file as a SINGLE atomic operation :
// every edit runs in order against the in-memory content (each sees the previous
// edit's result), and the file is written exactly once. If ANY edit fails to
// match, NOTHING is written — the file is left untouched and the failing edit's
// index (plus closest-match hints) is reported. This is the agent-grade
// "rewrite a function across N spots" primitive, crash-safe end to end.
func (m *Module) multiEdit(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p multiEditParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	p.Path = effectivePath(p.Path, p.FilePath, p.Filename, p.File)
	if len(p.Edits) == 0 {
		err := errors.New("edits must not be empty")
		return errResult(err), err
	}
	abs, err := m.resolveCtx(ctx, p.Path)
	if err != nil {
		return errResult(err), err
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return errResult(err), err
	}
	original := string(src)
	cur := original
	total := 0
	applied := make([]map[string]any, 0, len(p.Edits))
	for i, e := range p.Edits {
		loc := editLocator{
			OldString: e.OldString, NewString: e.NewString, ReplaceAll: bool(e.ReplaceAll),
			Occurrence: int(e.Occurrence), StartLine: int(e.StartLine), EndLine: int(e.EndLine),
			InsertAfter: e.InsertAfter, InsertBefore: e.InsertBefore,
			Prepend: bool(e.Prepend), Append: bool(e.Append), Expect: e.Expect,
		}
		upd, count, strategy, aerr := resolveEditOp(cur, loc)
		if aerr != nil {
			// All-or-nothing : a single failure aborts the whole batch with the
			// file untouched, so the agent never lands a half-applied edit.
			msg := fmt.Sprintf("edits[%d] in %s: %v", i, p.Path, aerr)
			data := map[string]any{"error": msg, "path": p.Path, "failed_edit": i}
			if ee, ok := aerr.(*editError); ok && ee.kind == "not_found" && len(ee.closest) > 0 {
				data["closest_matches"] = ee.closest
			}
			return tool.Result{Success: false, Error: msg, Data: data}, aerr
		}
		cur = upd
		total += count
		applied = append(applied, map[string]any{"index": i, "replacements": count, "strategy": strategy})
	}
	if cur == original {
		return tool.Result{Success: true, Data: map[string]any{"path": p.Path, "replacements": 0, "note": "no change"}}, nil
	}
	d := computeDiff(p.Path, original, cur)
	if p.DryRun {
		data := map[string]any{
			"path": p.Path, "edits": applied, "replacements": total, "dry_run": true,
			"additions": d.Added, "deletions": d.Removed,
			"note": "DRY RUN — nothing written. Re-call without dry_run to apply.",
		}
		if d.Unified != "" {
			data["diff"] = d.Unified
		}
		return tool.Result{Success: true, Data: data, Diff: diffView(p.Path, original, cur)}, nil
	}
	if err := atomicWrite(abs, []byte(cur), fileMode(abs, 0o644)); err != nil {
		return errResult(err), err
	}
	tindexes.markDirty(abs)
	notifyFileChange(ctx) // live workspace push (non-blocking, best-effort)
	return tool.Result{Success: true, Data: map[string]any{
		"path": p.Path, "edits": applied, "replacements": total,
		"additions": d.Added, "deletions": d.Removed,
	}, Diff: diffView(p.Path, original, cur)}, nil
}

type deleteParams struct {
	Path     string `json:"path"`
	FilePath string `json:"file_path"`
	Filename string `json:"filename"`
	File     string `json:"file"`
}

// delete removes a single file from the workspace. It refuses directories and
// errors on a missing path (so a delete never silently no-ops), then fires the
// live workspace push like the other mutators. This is the new-world equivalent
// of the legacy workspace.delete tool (filesystem is its alias).
func (m *Module) delete(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p deleteParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	p.Path = effectivePath(p.Path, p.FilePath, p.Filename, p.File)
	if strings.TrimSpace(p.Path) == "" {
		err := errors.New("delete: 'path' is required")
		return errResult(err), err
	}
	abs, err := m.resolveCtx(ctx, p.Path)
	if err != nil {
		return errResult(err), err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			e := fmt.Errorf("%s: no such file under the workspace", p.Path)
			return errResult(e), e
		}
		return errResult(err), err
	}
	if fi.IsDir() {
		e := fmt.Errorf("%s is a directory — delete only removes files", p.Path)
		return errResult(e), e
	}
	if err := os.Remove(abs); err != nil {
		return errResult(err), err
	}
	tindexes.markDirty(abs)
	notifyFileChange(ctx) // live workspace push (non-blocking, best-effort)
	return tool.Result{Success: true, Data: map[string]any{"path": p.Path, "deleted": true}}, nil
}

// applyFuzzySpans rebuilds content with each located span replaced by the
// (re-indented) replacement. Spans are ascending and non-overlapping, so a
// single gap-copy pass is correct and O(n).
func applyFuzzySpans(content string, spans []matchSpan, newStr string) string {
	var b strings.Builder
	b.Grow(len(content) + len(newStr))
	prev := 0
	for _, s := range spans {
		b.WriteString(content[prev:s.start])
		b.WriteString(reindentReplacement(newStr, s.indent))
		prev = s.end
	}
	b.WriteString(content[prev:])
	return b.String()
}

// flexInt accepts a JSON number OR a string ("500", "500.0"), null, or "" — LLMs
// routinely send numeric tool params as strings, which a plain int rejects and
// the whole tool call then fails ("cannot unmarshal string into ... of type
// int"). Unparseable input degrades to 0 so a caller default can take over.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(strings.Trim(strings.TrimSpace(string(b)), `"`))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) {
		*f = 0
		return nil
	}
	// Clamp into a safe int range BEFORE the conversion: a negative value (e.g.
	// "-5") would skip downstream "<= 0 → default" guards inconsistently, and a
	// huge one ("1e20") makes int(v) implementation-defined (can wrap negative,
	// then panic make([],0,neg) downstream). MaxInt32 is far beyond any real
	// offset/limit/context.
	switch {
	case v < 0:
		*f = 0
	case v > math.MaxInt32:
		*f = math.MaxInt32
	default:
		*f = flexInt(int(v))
	}
	return nil
}

// flexBool accepts a JSON bool, OR a string ("true"/"false"/"1"/"0"/"yes"/"no"),
// OR a number (0/1), OR null — LLMs routinely send booleans as strings
// (outline:"true"), which a plain bool rejects and fails the whole call.
// Anything unrecognized degrades to false.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	s := strings.ToLower(strings.TrimSpace(strings.Trim(strings.TrimSpace(string(b)), `"`)))
	switch s {
	case "true", "1", "yes", "on":
		*f = true
	default:
		*f = false
	}
	return nil
}

type globParams struct {
	Pattern    string  `json:"pattern"`
	Glob       string  `json:"glob"`        // alias : models often key the pattern under the tool name
	Type       string  `json:"type"`        // "file" | "dir" | "any" (default)
	MaxResults flexInt `json:"max_results"` // cap (default globDefaultCap)
}

// effectivePattern resolves the glob pattern, accepting the common LLM mistake of
// keying it under the TOOL name ("glob") instead of the parameter name
// ("pattern"). Returning the alias rather than erroring keeps a confused model
// from dead-ending the agent on "pattern must not be empty".
func (p globParams) effectivePattern() string {
	if s := strings.TrimSpace(p.Pattern); s != "" {
		return s
	}
	return strings.TrimSpace(p.Glob)
}

// glob walks the confined tree and returns paths matching the pattern, with full
// ** recursion (the stdlib filepath.Glob cannot recurse), VCS/build noise dirs
// pruned, an optional file/dir type filter, results sorted MOST-RECENT-FIRST
// (the order an agent usually wants), and a cap with a truncation flag.
func (m *Module) glob(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p globParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	patternIn := p.effectivePattern()
	if patternIn == "" {
		err := errors.New("pattern must not be empty")
		return errResult(err), err
	}
	base, ok := m.globBase(ctx)
	if !ok {
		// Policy present but no workdir → confine to nothing rather than
		// leaking the daemon CWD.
		return tool.Result{Success: true, Data: map[string]any{"files": []string{}, "count": 0, "truncated": false}}, nil
	}
	realBase := base
	if rb, e := filepath.EvalSymlinks(base); e == nil {
		realBase = rb
	}
	pp, hasPolicy := workdir.PathPolicyFromContext(ctx)
	maxResults := int(p.MaxResults)
	if maxResults <= 0 {
		maxResults = globDefaultCap
	}
	pattern := filepath.ToSlash(patternIn)
	ignore := loadGitignore(realBase)

	type hit struct {
		rel   string
		mtime int64
	}
	var hits []hit
	_ = filepath.WalkDir(realBase, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry : skip, never abort the walk
		}
		if d.IsDir() {
			if _, skip := skipDirs[d.Name()]; skip && abs != realBase {
				return filepath.SkipDir
			}
			if ignore != nil && abs != realBase {
				if rel, ok := relUnder(realBase, abs); ok && ignore.ignored(rel, true) {
					return filepath.SkipDir // .gitignore'd directory
				}
			}
		}
		if abs == realBase {
			return nil
		}
		if ignore != nil && !d.IsDir() {
			if rel, ok := relUnder(realBase, abs); ok && ignore.ignored(rel, false) {
				return nil // .gitignore'd file
			}
		}
		switch p.Type {
		case "file":
			if d.IsDir() {
				return nil
			}
		case "dir":
			if !d.IsDir() {
				return nil
			}
		}
		// Symlink-safe confinement, mirroring grep : resolve then check membership
		// so a planted symlink pointing outside the workdir is dropped.
		real := abs
		if hasPolicy {
			r, e := pp.Enforce(abs)
			if e != nil {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			real = r
		} else if r, e := filepath.EvalSymlinks(abs); e == nil {
			real = r
		}
		rel, in := relInside(realBase, real)
		if !in || rel == "." || !matchGlob(pattern, rel) {
			return nil
		}
		var mt int64
		if info, e := d.Info(); e == nil {
			mt = info.ModTime().UnixNano()
		}
		hits = append(hits, hit{rel, mt})
		return nil
	})

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].mtime != hits[j].mtime {
			return hits[i].mtime > hits[j].mtime // most recent first
		}
		return hits[i].rel < hits[j].rel
	})
	truncated := false
	if len(hits) > maxResults {
		hits = hits[:maxResults]
		truncated = true
	}
	files := make([]string, len(hits))
	for i, h := range hits {
		files[i] = h.rel
	}
	return tool.Result{Success: true, Data: map[string]any{"files": files, "count": len(files), "truncated": truncated}}, nil
}

type grepParams struct {
	Pattern    string  `json:"pattern"`
	Grep       string  `json:"grep"` // alias : models often key the regex under the tool name
	Path       string  `json:"path"`
	Include    string  `json:"include"`
	MaxResults flexInt `json:"max_results"`
	Context    flexInt `json:"context"`     // lines of context around each match (0-20)
	OutputMode string  `json:"output_mode"` // "content" | "files_with_matches" | "count"
	Multiline  bool    `json:"multiline"`   // match across line boundaries
	Semantic   string  `json:"semantic"`    // "auto" (default) | "on" | "off" : fuse code-semantic matches
}

func (m *Module) grep(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p grepParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	// Accept the regex keyed under the tool name ("grep") — the same alias models
	// reach for on glob — so a confused model doesn't dead-end the agent.
	if strings.TrimSpace(p.Pattern) == "" {
		p.Pattern = strings.TrimSpace(p.Grep)
	}
	if strings.TrimSpace(p.Pattern) == "" {
		err := errors.New("pattern must not be empty")
		return errResult(err), err
	}
	if p.MaxResults <= 0 {
		p.MaxResults = 1000
	}
	switch {
	case p.Context < 0:
		p.Context = 0
	case p.Context > 20:
		p.Context = 20
	}
	if p.Path == "" {
		p.Path = "."
	}
	root, err := m.resolveCtx(ctx, p.Path)
	if err != nil {
		return errResult(err), err
	}
	// Display base : matches are reported relative to the workdir root.
	base, ok := m.globBase(ctx)
	if !ok {
		base = root
	}
	if rb, e := filepath.EvalSymlinks(base); e == nil {
		base = rb
	}
	re, literal, err := compilePattern(p.Pattern, p.Multiline)
	if err != nil {
		return errResult(fmt.Errorf("invalid pattern: %w", err)), err
	}
	pp, hasPolicy := workdir.PathPolicyFromContext(ctx)

	mode := grepOutput(p.OutputMode)
	switch mode {
	case grepContent, grepFiles, grepCount:
	default:
		mode = grepContent
	}

	req := grepRequest{
		root:        root,
		base:        base,
		ignore:      loadGitignore(base),
		include:     p.Include,
		re:          re,
		literal:     literal,
		mode:        mode,
		contextN:    int(p.Context),
		maxResults:  int(p.MaxResults),
		maxFileSize: m.cfg.MaxFileBytes,
		// confine runs in the single producer BEFORE a file is ever opened, so
		// an out-of-workdir target (incl. a symlink escape) is never read.
		confine: func(abs string) bool {
			if hasPolicy {
				_, e := pp.Enforce(abs)
				return e == nil
			}
			real := abs
			if r, e := filepath.EvalSymlinks(abs); e == nil {
				real = r
			}
			_, in := relInside(base, real)
			return in
		},
		rel: func(abs string) (string, bool) {
			real := abs
			if r, e := filepath.EvalSymlinks(abs); e == nil {
				real = r
			}
			return relInside(base, real)
		},
	}
	// Trigram fast path : ask the per-root index for the handful of files that
	// could match, then confirm only those. Falls back to a full parallel scan
	// when no index is ready yet or the pattern yields no trigram narrowing.
	idx := tindexes.get(root, m.cfg.MaxFileBytes)
	idx.maybeBuild()
	enum := walkEnum(req)
	if cand, usable := idx.candidates(p.Pattern); usable {
		enum = listEnum(req, cand)
	}
	res, err := runGrep(ctx, req, enum)
	if err != nil {
		return errResult(err), err
	}
	data := map[string]any{"truncated": res.Truncated, "scanned": res.Scanned}
	switch mode {
	case grepFiles:
		data["files"] = res.Files
	case grepCount:
		data["count"] = res.Count
	default:
		data["matches"] = res.Matches
	}
	// Native code intelligence : when the app enables auto_index_workdir,
	// fuse the semantically-nearest code chunks (ephemeral per-workdir
	// index, built off-loop, LRU+TTL bounded) alongside the exact matches.
	if mode == grepContent && strings.ToLower(p.Semantic) != "off" {
		if model, on := codeIndexConfig(ctx); on {
			if emb := module.EmbedderFrom(ctx); emb != nil {
				si := sindexes.get(root, m.cfg.MaxFileBytes)
				si.maybeBuild(emb, model)
				if rel := si.search(ctx, emb, model, p.Pattern, 8); len(rel) > 0 {
					data["related"] = rel
				}
			}
		}
	}
	return tool.Result{Success: true, Data: data}, nil
}

// codeIndexConfig reads the app's per-module config : whether the
// ephemeral workdir semantic index is enabled and which code embedding
// model to use (default jina-code).
func codeIndexConfig(ctx context.Context) (model string, enabled bool) {
	cfg := module.ModuleConfigFrom(ctx)
	if cfg == nil {
		return "", false
	}
	if v, ok := cfg["auto_index_workdir"].(bool); ok {
		enabled = v
	}
	model = "code"
	if s, ok := cfg["code_embed_model"].(string); ok && strings.TrimSpace(s) != "" {
		model = strings.TrimSpace(s)
	}
	return model, enabled
}

func errResult(err error) tool.Result {
	return tool.Result{Success: false, Error: err.Error()}
}

// notifyFileChange signals the live workspace notifier that the agent just
// mutated a file in the session workdir, so the daemon can push a coalesced
// workspace-changes event to the client. Non-blocking and best-effort : it
// fires only when a notifier, a caller identity, and a real workdir all ride on
// ctx (the agent path) — setup / CLI / test calls have none and skip silently.
// Never returns an error : a failed live push must never affect the write.
func notifyFileChange(ctx context.Context) {
	workdir.NotifyFileChange(ctx)
}
