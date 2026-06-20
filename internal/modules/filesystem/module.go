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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/flexjson"
	"github.com/mbathepaul/digitorn/internal/runtime/context/repomap"
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

	// semanticApps tracks appIDs where semantic search is confirmed active
	// (auto_index_workdir=true AND embedder available). Populated on each
	// dispatch so DynamicToolPrompts can inject the right grep guidance.
	semanticApps sync.Map   // appID (string) → struct{}
	embedderSeen atomic.Bool // true once an embedder was observed in any ctx
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

// Invoke intercepts every tool dispatch to snapshot per-app semantic search
// availability, then delegates to Base.Invoke. This keeps DynamicToolPrompts
// accurate without requiring a separate wiring call.
func (m *Module) Invoke(ctx context.Context, name string, params []byte) (tool.Result, error) {
	m.snapshotSemanticStatus(ctx)
	return m.Base.Invoke(ctx, name, params)
}

// snapshotSemanticStatus reads the current dispatch context and caches whether
// semantic search is active for this app. Called on every tool invocation so
// the status is always fresh by the time the next turn's prompt is assembled.
func (m *Module) snapshotSemanticStatus(ctx context.Context) {
	id, ok := tool.IdentityFromContext(ctx)
	if !ok || id.AppID == "" {
		return
	}
	cfg := module.ModuleConfigFrom(ctx)
	autoIndex, _ := cfg["auto_index_workdir"].(bool)
	emb := module.EmbedderFrom(ctx)
	if emb != nil {
		m.embedderSeen.Store(true)
	}
	if autoIndex && emb != nil {
		m.semanticApps.Store(id.AppID, struct{}{})
	}
}

// DynamicToolPrompts injects an enhanced grep prompt when semantic search is
// confirmed active for this app (auto_index_workdir=true + embedder running).
// The overlay is additive — the static ToolPrompt still applies.
func (m *Module) DynamicToolPrompts(scope domainmodule.PromptScope) map[string]string {
	if scope.AppID == "" {
		return nil
	}
	if _, ok := m.semanticApps.Load(scope.AppID); !ok {
		return nil
	}
	return map[string]string{
		"filesystem.grep": semanticGrepPrompt,
	}
}

const semanticGrepPrompt = `
SEMANTIC SEARCH IS ACTIVE (ONNX vector embeddings operational).
grep returns TWO result sets fused together:
  1. "matches" — exact RE2 hits (trigram-indexed, instant).
  2. "related" — semantically similar code chunks ranked by vector score,
     even when they share NO common text with your pattern.

Each "related" hit includes:
  • snippet  — the relevant code block
  • symbol   — the enclosing function/type
  • callers  — who calls that symbol (call graph, no extra grep needed)
  • imports  — the file's imports

When to exploit "related":
  • Conceptual search: grep("handles authentication") → finds auth code even if "authentication" doesn't appear literally
  • Find similar implementations: grep("retry with backoff") → finds all retry patterns
  • Cross-language patterns: grep("rate limit") → finds limiters in any language
  • Alternative approaches: grep("parse JSON") → finds all JSON parsers

Workflow: run ONE grep with a descriptive phrase, read both "matches" and "related", then act. Skip the "find files → open each → search" loop entirely.`

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
		Description: "Read a file with line numbers (cat -n style). Read a DIRECTORY (e.g. \".\" for the project root) to get a .gitignore-aware TREE of its structure — the way to orient yourself in an unfamiliar project. IMAGES (PNG/JPG/GIF/WEBP/BMP) are returned as actual visual content you can SEE, not described. Use `outline: true` on a code file OR a directory to get a structural map (functions/classes/headings + line numbers) instead of full content — then read a precise line range or edit by line number. Pass `paths` (a list) to read several files (and/or images) in one call.",
		ToolPrompt: "DIRECTORY NAVIGATION — `read .` is your starting point for any unfamiliar project:\n" +
			"  `read .`              → ENRICHED TREE: every file with [package] NLines KeySymbols, every directory with (N files · N lines) totals. Jump directly to any file with Read(path, offset=N).\n" +
			"  `read . outline:true` → ALL SYMBOLS with line numbers across every file (up to 2000 files). Use this when you need to find any function/type without grep.\n" +
			"  `read <subdir>`       → Same enriched tree scoped to a subtree.\n" +
			"These two commands replace 90% of grep/glob use cases for navigation — prefer them.\n\n" +
			"FILE READING:\n" +
			"Read before you edit or write — never edit a file blind. When you already know the symbol or region, read just that range (offset/limit) rather than the whole file; on a large file run `outline: true` first to map it, then read the precise lines.\n" +
			"Batch related files in ONE call via `paths` instead of many sequential reads.\n" +
			"The line numbers in the output are authoritative: cite locations as path:line and edit by those numbers.\n" +
			"READ OUTPUT FORMAT: every content line is prefixed with its 1-based line number and a TAB. Example:\n" +
			"  1\tpackage main\n" +
			"  2\t\n" +
			"  3\tfunc main() {\n" +
			"  The line number and tab are PURE DISPLAY — NOT part of the actual file content.\n" +
			"  When you call `edit` or `write`, NEVER include line numbers or tabs in old_string / new_string / content.\n" +
			"Do NOT re-read a file you just wrote or edited to confirm — the write/edit tool already errors on failure.",
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
		ToolPrompt: "Use `write` for a NEW file or a deliberate full rewrite — it replaces the entire file atomically (crash-safe). To change part of an existing file use `edit`/`multi_edit` instead: surgical edits are faster, safer, and never drop code by accident.\n" +
			"If the file already exists, read it first — never clobber content you didn't mean to touch.\n" +
			"\n" +
			"CRITICAL — content encoding rules (violations corrupt the file silently):\n" +
			"• `content` MUST be a plain JSON string — a single quoted value, never an array [], never an object {}.\n" +
			"• Wrong: content: [{\"color\":\"red\"}]  ← array, will be mangled\n" +
			"• Wrong: content: {\"body\":\"...\"}      ← object, will be mangled\n" +
			"• Right: content: \"body { color: red; }\" ← plain string, always correct\n" +
			"• CSS, TOML, YAML, JSX, JSON-inside-a-file: all go as a plain string. The { } : -- @ characters inside the string are fine — they are NOT JSON syntax once inside quotes.\n" +
			"• Never pre-encode or escape the content yourself. Pass the raw file body as-is.\n" +
			"\n" +
			"Style rules:\n" +
			"• Match the surrounding code's indentation, quotes, and conventions exactly.\n" +
			"• Never write credentials, API keys, or secrets into source.\n" +
			"• Preserve existing file encoding (UTF-8 unless the file is explicitly otherwise).",
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
		ToolPrompt: "DEFAULT: use start_line/end_line. It never fails — you give numbers, not text.\n" +
			"old_string is for quick single-line fixes only when you have the EXACT text fresh from a read in the SAME step.\n\n" +
			"RULE: if you are not 100% sure of every character in old_string (whitespace, quotes, indentation), use start_line/end_line instead. Reconstructing text from memory causes failures.\n\n" +
			"HOW TO GET LINE NUMBERS — pick the fastest source:\n" +
			"• Code index in the system prompt: `L302-L450 func Run(...)` → start_line=302, end_line=450\n" +
			"• grep with context:3 → the line numbers are in the output\n" +
			"• read(path, outline=true) → every symbol with its line range\n" +
			"• read(path, offset=N, limit=M) → content with line numbers; use those numbers directly\n\n" +
			"WHEN old_string IS SAFE:\n" +
			"• You just ran read/grep in this SAME tool-call batch and are copy-pasting the exact output — not retyping it\n" +
			"• The change is a single short line with no ambiguous whitespace\n" +
			"• old_string must be unique in the file; add context lines or use occurrence:N if it appears multiple times\n\n" +
			"NEVER include the line-number prefix from read ('  142\\t') in old_string — strip it first.\n" +
			"dry_run:true previews the diff before writing. expect:\"snippet\" guards against editing a stale version.",
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
		Description: "Find paths matching a glob pattern (supports ** for recursion), newest first. VCS/build dirs are skipped. Pass `tree: true` to get an ENRICHED tree: each file shows its package, line count, and key symbols — use this to understand a subtree without reading files one by one. To understand the full project layout, prefer `read .` (complete enriched tree with metadata) or `read . outline:true` (all definitions per file with line numbers).",
		ToolPrompt: "Reach for `glob` when you know the NAME or path shape (\"**/*.go\", \"src/**/*.{ts,tsx}\", \"cmd/*/main.go\"); reach for `grep` when you know the CONTENT.\n" +
			"Full glob syntax: recursive **, brace alternation {a,b}, ranges, character classes. Results are newest-first — doubles as \"what changed recently\".\n" +
			"ALWAYS use `tree: true` for any multi-file result: the output is an ENRICHED tree showing [package]  NLines  KeySymbols for every file, and (N files · N lines) totals per directory.\n" +
			"Example: glob(\"internal/runtime/**/*.go\", tree: true) → shows every file in that subtree with its package, line count, and top symbols — no extra reads needed.\n" +
			"To orient in an unfamiliar project: `read .` gives the full enriched project tree (package + lines + symbols per file, directory totals). `read . outline:true` gives every symbol with line numbers across all files.\n" +
			"Skip glob for orientation — `read .` is always better than `glob(\"**/*\", tree: true)` because it adds directory aggregate stats.",
		Params: []tool.ParamSpec{
			{Name: "pattern", Type: "string", Description: "Glob pattern, e.g. \"**/*.go\" or \"src/*.ts\".", Required: true},
			{Name: "type", Type: "string", Description: "Filter: \"file\", \"dir\", or \"any\" (default).", Default: "any"},
			{Name: "max_results", Type: "integer", Description: "Cap on matches (default 10000). Only lower this if you want a short preview.", Default: 0},
			{Name: "tree", Type: "boolean", Description: "Render as enriched tree: each file shows [package] NLines KeySymbols, directories show (N files · N lines) totals. Default true — pass false only when you need a raw path list.", Default: true},
		},
		RiskLevel: tool.RiskLow,
		Handler:   m.glob,
	})

	m.RegisterTool(module.Tool{
		Name:        "grep",
		Description: "Search file contents: exact RE2 regex (trigram-indexed, instant) AND semantic vector search (ONNX embeddings, when auto_index_workdir is enabled). Returns exact \"matches\" + semantically similar \"related\" chunks with callers and imports — one grep call replaces many read+search cycles.",
		ToolPrompt: "Your primary way to locate code by content. TWO search engines run in parallel:\n" +
			"  1. EXACT (always): RE2 regex, trigram-indexed — O(matches) not O(files).\n" +
			"  2. SEMANTIC (active when auto_index_workdir=true + embedder running): ONNX vector search returns a `related` field with the most similar code chunks ranked by score, even with zero text overlap with your pattern.\n\n" +
			"The `related` field contains: snippet · enclosing symbol · callers (call graph) · imports. One grep = exact hits + semantic context + callers. No follow-up reads needed in most cases.\n\n" +
			"HOW TO USE:\n" +
			"• SCOPE: set `include` (\"*.go\", \"*.{ts,tsx}\") and `path` (subtree) so results stay sharp.\n" +
			"• FIND FIRST: output_mode \"files_with_matches\" → cheap list of matching files; \"count\" → tallies per file. Default \"content\" → the lines.\n" +
			"• SEE CONTEXT: `context: 3` shows surrounding lines — understand the hit without a separate read.\n" +
			"• SEMANTIC PATTERNS: use a descriptive phrase like \"handles rate limiting\", \"parses JWT\", \"retries with backoff\" — the vector engine finds conceptually matching code even if the exact words don't appear.\n" +
			"• FLAGS: `ignore_case`, `multiline`, or inline RE2 flags (?i) (?m) (?s).\n" +
			"• LITERALS: invalid regex (Foo(, a[i], /path/) is auto-escaped and searched as literal text.\n" +
			"• CALL GRAPH: each match already carries its callers — follow the thread without more greps.\n" +
			"Note: RE2 has no lookbehind/backreferences. For those edge cases: bash + `rg -P`.",
		Params: []tool.ParamSpec{
			{Name: "pattern", Type: "string", Description: "RE2 regular expression. Inline flags supported: (?i) case-insensitive, (?m) multiline ^/$, (?s) dot matches newline.", Required: true},
			{Name: "path", Type: "string", Description: "Directory (or file) to search under (default: workspace root).", Default: ".", Path: true},
			{Name: "include", Type: "string", Description: "Glob to scope files, e.g. \"*.go\" or \"*.{ts,tsx}\".", Default: ""},
			{Name: "output_mode", Type: "string", Description: "\"content\" (matching lines, default), \"files_with_matches\" (just the paths — find WHERE fast), or \"count\" (match counts per file).", Default: "content"},
			{Name: "context", Type: "integer", Description: "Lines of surrounding context shown around each match (default 3, max 20). Use a higher value when you need to see more of the function to write new_string.", Default: 3},
			{Name: "ignore_case", Type: "boolean", Description: "Case-insensitive match.", Default: false},
			{Name: "multiline", Type: "boolean", Description: "Let the pattern match across line boundaries (a single match can span lines).", Default: false},
			{Name: "max_results", Type: "integer", Description: "Cap on match count.", Default: 500},
			{Name: "ast_pattern", Type: "string", Description: "Structural symbol search via treesitter AST. Space-separated tokens ALL matched against each symbol's body. Returns full symbol bodies in `ast_matches`. Example: \"context.Context error\" finds every function taking a context that returns an error. Runs in parallel, never slows exact search.", Default: ""},
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
	var abs string
	var err error
	if pp, ok := workdir.PathPolicyFromContext(ctx); ok {
		abs, err = pp.Enforce(target)
	} else {
		abs, err = m.resolve(target)
	}
	if err != nil {
		return "", err
	}
	if isProtectedFile(abs) {
		return "", fmt.Errorf("access denied: %s is an internal Digitorn configuration file", target)
	}
	return abs, nil
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
	Offset   flexjson.Int  `json:"offset"`    // 1-based start line (default 1)
	Limit    flexjson.Int  `json:"limit"`     // max lines to return (default 2000)
	Outline  flexjson.Bool `json:"outline"`   // return a structural map (defs + line numbers) not content
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
		// A directory orients the agent instead of erroring : `outline: true` →
		// a cross-file map of every file's definitions ; otherwise a bounded,
		// .gitignore-aware TREE of the structure. Both are pure-Go (no treesitter).
		label := rel
		if label == "" || label == "." {
			label = "."
		}
		if p.Outline {
			return fmt.Sprintf("Outline of %s (definitions per file):\n\n%s", label, renderDirOutline(abs)), nil, nil
		}
		return fmt.Sprintf("Directory %s (tree; files are read individually):\n\n%s", label, renderDirTree(abs)), nil, nil
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
	Path     string      `json:"path"`
	FilePath string      `json:"file_path"`
	Filename string      `json:"filename"`
	File     string      `json:"file"`
	Content  flexjson.Content `json:"content"`
}

var reLineNumber = regexp.MustCompile(`^\s*\d+\t`)

func stripLineNumbers(s string) string {
	lines := strings.Split(s, "\n")
	allNumbered := true
	for _, l := range lines {
		if l == "" {
			continue
		}
		if !reLineNumber.MatchString(l) {
			allNumbered = false
			break
		}
	}
	if !allNumbered {
		return s
	}
	result := make([]string, len(lines))
	for i, l := range lines {
		result[i] = reLineNumber.ReplaceAllString(l, "")
	}
	return strings.Join(result, "\n")
}

func (m *Module) write(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var intermediate map[string]json.RawMessage
	if err := json.Unmarshal(raw, &intermediate); err != nil {
		return errResult(err), err
	}
	normalized, err := json.Marshal(intermediate)
	if err != nil {
		return errResult(err), err
	}

	var p writeParams
	if err := json.Unmarshal(normalized, &p); err != nil {
		return errResult(err), err
	}

	p.Content = flexjson.Content(stripLineNumbers(string(p.Content)))

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
		if fi.Size() <= diffContentCap {
			if b, e := os.ReadFile(abs); e == nil {
				oldContent = string(b)
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return errResult(err), err
	}
	if err := atomicWrite(abs, []byte(string(p.Content)), fileMode(abs, 0o644)); err != nil {
		return errResult(err), err
	}
	tindexes.markDirty(abs)
	sindexes.markDirty(abs)
	repomap.MarkDirty(abs)
	notifyFileChange(ctx)
	action := "created"
	if existed {
		action = "overwrote"
	}
	return tool.Result{
		Success: true,
		Data:    map[string]any{"path": p.Path, "bytes": len(string(p.Content)), "action": action},
		Diff:    diffView(p.Path, oldContent, string(p.Content)),
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
	ReplaceAll flexjson.Bool `json:"replace_all"`
	// Surgical locators (alternatives to old_string — provide exactly one).
	Occurrence   flexjson.Int  `json:"occurrence"`
	StartLine    flexjson.Int  `json:"start_line"`
	EndLine      flexjson.Int  `json:"end_line"`
	InsertAfter  string   `json:"insert_after"`
	InsertBefore string   `json:"insert_before"`
	Prepend      flexjson.Bool `json:"prepend"`
	Append       flexjson.Bool `json:"append"`
	Expect       string   `json:"expect"`
	DryRun       flexjson.Bool `json:"dry_run"`
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
	return applyEditTry(content, oldStr, newStr, replaceAll, true)
}

// applyEditTry is applyEdit with one self-healing retry: on a TOTAL miss it
// strips read's line-number prefixes ("  142\t…") from old_string — the single
// most common reason a model's edit fails first-try is pasting numbered read
// output verbatim — and tries once more. allowStrip is false on the retry so it
// can never loop. The strip is conservative (every non-blank line must carry the
// prefix), so a genuine edit is never mangled.
func applyEditTry(content, oldStr, newStr string, replaceAll, allowStrip bool) (updated string, count int, strategy string, err error) {
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
		if allowStrip {
			if s := stripReadLineNumbers(oldStr); s != oldStr && s != "" {
				return applyEditTry(content, s, newStr, replaceAll, false)
			}
		}
		cms := closestMatches(content, oldStr, 3)
		hint := "old_string not found in the file."
		if len(cms) > 0 {
			hint += fmt.Sprintf(
				" IMMEDIATE FIX: copy closest_matches[0].preview VERBATIM as your new old_string — do not retype it, do not paraphrase it. The preview is the exact text at L%d-L%d (similarity %.0f%%). Alternatively use start_line=%d end_line=%d.",
				cms[0].StartLine, cms[0].EndLine, cms[0].Similarity*100, cms[0].StartLine, cms[0].EndLine)
		} else {
			hint += " No close match found. Likely causes: (1) line-number prefix from `read` included — strip it; (2) file changed — re-read the target lines; (3) use start_line/end_line locator instead of old_string."
		}
		return "", 0, "", &editError{kind: "not_found", message: hint, closest: cms}
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
	sindexes.markDirty(abs)
	repomap.MarkDirty(abs)
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
		ReplaceAll   flexjson.Bool `json:"replace_all"`
		Occurrence   flexjson.Int  `json:"occurrence"`
		StartLine    flexjson.Int  `json:"start_line"`
		EndLine      flexjson.Int  `json:"end_line"`
		InsertAfter  string   `json:"insert_after"`
		InsertBefore string   `json:"insert_before"`
		Prepend      flexjson.Bool `json:"prepend"`
		Append       flexjson.Bool `json:"append"`
		Expect       string   `json:"expect"`
	} `json:"edits"`
	DryRun flexjson.Bool `json:"dry_run"`
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
	sindexes.markDirty(abs)
	repomap.MarkDirty(abs)
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

type globParams struct {
	Pattern    string  `json:"pattern"`
	Glob       string  `json:"glob"`        // alias : models often key the pattern under the tool name
	Type       string  `json:"type"`        // "file" | "dir" | "any" (default)
	MaxResults flexjson.Int `json:"max_results"` // cap (default globDefaultCap)
	Tree       *bool   `json:"tree"`        // nil = default true; false only when explicitly set
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
			if isSkipped(d.Name(), abs) && abs != realBase {
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
	if p.Tree == nil || *p.Tree {
		var treeOut string
		if base != "" {
			treeOut = renderPathsTreeRich(base, files)
		} else {
			treeOut = renderPathsTree(files)
		}
		return tool.Result{Success: true, Data: map[string]any{"tree": treeOut, "count": len(files), "truncated": truncated}}, nil
	}
	return tool.Result{Success: true, Data: map[string]any{"files": files, "count": len(files), "truncated": truncated}}, nil
}

// astHit is one AST structural-search result returned in the "ast_matches"
// field. Defined here (no build tag) so the grep handler can use the type
// regardless of whether treesitter is compiled in.
type astHit struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Symbol  string `json:"symbol"`
	Kind    string `json:"kind"`
	Sig     string `json:"sig"`
	Snippet string `json:"snippet"` // first ~400 chars of the symbol body
}

type grepParams struct {
	Pattern    string  `json:"pattern"`
	Grep       string  `json:"grep"`        // alias : models often key the regex under the tool name
	Path       string  `json:"path"`
	Include    string  `json:"include"`
	MaxResults flexjson.Int `json:"max_results"`
	Context    flexjson.Int `json:"context"`     // lines of context around each match (0-20)
	OutputMode string  `json:"output_mode"` // "content" | "files_with_matches" | "count"
	Multiline  flexjson.Bool    `json:"multiline"`   // match across line boundaries
	IgnoreCase flexjson.Bool    `json:"ignore_case"` // case-insensitive match
	Semantic   string  `json:"semantic"`    // "auto" (default) | "on" | "off" : fuse code-semantic matches
	ASTPattern string  `json:"ast_pattern"` // structural symbol search via treesitter
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
	case p.Context <= 0:
		p.Context = 3 // default 3 lines of context (like grep -C3)
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
	pat := p.Pattern
	if p.IgnoreCase {
		// (?i) forces the regexp path (no literal fast-path) and folds case for
		// both literal and regex patterns — RE2-safe, no backtracking.
		pat = "(?i)" + pat
	}
	re, literal, err := compilePattern(pat, bool(p.Multiline))
	literalFallback := false
	if err != nil {
		// The pattern isn't valid regex — almost always a LITERAL that happens to
		// contain regex metacharacters: a call like `Foo(`, a slice index `a[i]`, a
		// path. Match the ORIGINAL text literally (QuoteMeta escapes the metachars),
		// keeping the case / multiline flags, so the search succeeds instead of
		// dead-ending the agent on an unmatched paren or bracket.
		flags := "m"
		if p.Multiline {
			flags += "s"
		}
		if p.IgnoreCase {
			flags += "i"
		}
		re, err = regexp.Compile("(?" + flags + ")" + regexp.QuoteMeta(p.Pattern))
		literal = nil
		literalFallback = true
	}
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
	// Structural AST search — treesitter symbol scan, parallel, 500ms budget.
	// Returns full symbol bodies in "ast_matches". No-op when ast_pattern is empty
	// or treesitter is not compiled in (stub returns nil immediately).
	var astCh chan []astHit
	if strings.TrimSpace(p.ASTPattern) != "" {
		astCh = make(chan []astHit, 1)
		astRoot := root
		astPat := strings.TrimSpace(p.ASTPattern)
		astMax := int(p.MaxResults)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					astCh <- nil
				}
			}()
			astCh <- m.astSearch(ctx, astRoot, astPat, astMax)
		}()
	}

	// Code-intelligence enrichment is kicked IN PARALLEL with the exact scan
	// so it overlaps and never delays grep. Best-effort + recover-guarded +
	// bounded at the join : a missing / building / broken index just yields
	// exact-only. Off entirely unless the app enabled auto_index_workdir, so
	// the base grep path keeps its benchmark speed.
	var relCh chan []sHit
	if mode == grepContent && strings.ToLower(p.Semantic) != "off" {
		if model, on := codeIndexConfig(ctx); on {
			if emb := module.EmbedderFrom(ctx); emb != nil {
				relCh = make(chan []sHit, 1)
				go func() {
					defer func() { _ = recover() }()
					relCh <- m.codeEnrich(ctx, root, emb, model, p.Pattern)
				}()
			}
		}
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
	if literalFallback {
		data["note"] = "pattern wasn't valid regex (looks like literal text with metacharacters like '(' or '['); searched it as a literal string"
	}
	switch mode {
	case grepFiles:
		data["files"] = res.Files
	case grepCount:
		data["count"] = res.Count
	default:
		data["matches"] = res.Matches
		if len(res.Matches) > 0 {
			data["output"] = formatGrepOutput(res.Matches)
		}
	}
	// Join the parallel enrichment with a hard budget — the exact result is
	// already computed, so this never slows grep beyond the budget (and the
	// work overlapped the scan). Timeout / cancel → exact-only.
	if relCh != nil {
		select {
		case rel := <-relCh:
			if len(rel) > 0 {
				data["related"] = rel
			}
		case <-time.After(codeEnrichBudget):
		case <-ctx.Done():
		}
	}
	// Join AST structural search — 500ms budget (treesitter walk is slower).
	// Already overlapped with the exact scan + semantic enrichment above.
	if astCh != nil {
		select {
		case hits := <-astCh:
			if len(hits) > 0 {
				data["ast_matches"] = hits
			}
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	return tool.Result{Success: true, Data: data}, nil
}

// formatGrepOutput renders matches in bash-grep style: file:line:content with
// context lines and file separators. The agent can read line numbers directly
// and use them in edit(start_line=N) without parsing JSON fields.
func formatGrepOutput(matches []grepMatch) string {
	var b strings.Builder
	lastFile := ""
	for i, m := range matches {
		if m.File != lastFile {
			if lastFile != "" {
				b.WriteString("--\n")
			}
			lastFile = m.File
		}
		for j, line := range m.Before {
			ln := m.LineNum - len(m.Before) + j
			fmt.Fprintf(&b, "%s:%d: %s\n", m.File, ln, line)
		}
		fmt.Fprintf(&b, "%s:%d: %s\n", m.File, m.LineNum, m.Text)
		for j, line := range m.After {
			fmt.Fprintf(&b, "%s:%d: %s\n", m.File, m.LineNum+1+j, line)
		}
		if i < len(matches)-1 && matches[i+1].File == m.File {
			// same file, check if next match is a separate block
			next := matches[i+1]
			gap := next.LineNum - m.LineNum - len(m.After)
			if gap > 1 {
				b.WriteString("--\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// codeEnrichBudget caps how long grep waits for the parallel code-semantic
// enrichment after the exact scan. Bounded so grep never slows materially.
const codeEnrichBudget = 200 * time.Millisecond

// codeEnrich runs the semantic code search + graph context for a pattern.
// recover-guarded : any failure returns nil (grep stays exact-only).
// Pipeline: BM25-hybrid vector search → call-graph enrichment → cross-encoder rerank.
// Each step is independently recover-guarded and timeout-bounded: a slow or broken
// step skips silently, leaving the result from the previous step intact.
func (m *Module) codeEnrich(ctx context.Context, root string, emb module.Embedder, model, pattern string) (hits []sHit) {
	defer func() { _ = recover() }()
	si := sindexes.get(root, m.cfg.MaxFileBytes)
	si.maybeBuild(emb, model)
	// Fetch extra candidates so reranking has room to reorder (trimmed to 8 at end).
	hits = si.search(ctx, emb, model, pattern, 16)
	for i := range hits {
		sc := codeContextFor(root, m.cfg.MaxFileBytes, hits[i].Path, hits[i].Line)
		if hits[i].Symbol == "" {
			hits[i].Symbol = sc.Symbol
		}
		hits[i].Callers = sc.Callers
		hits[i].Imports = sc.Imports
	}

	// Cross-encoder reranking — hard 50ms budget, any failure keeps original order.
	// The reranker (BGE-reranker-base, ONNX) is injected by the daemon dispatcher
	// and auto-downloads on first use. Never blocks the caller beyond the budget.
	if reranker := module.RerankerFrom(ctx); reranker != nil && len(hits) > 1 {
		func() {
			defer func() { _ = recover() }()
			rctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
			docs := make([]string, len(hits))
			for i, h := range hits {
				docs[i] = h.Snippet
			}
			scores, err := reranker.Rerank(rctx, "bge-reranker-base", pattern, docs)
			if err != nil || len(scores) != len(hits) {
				return // keep original order
			}
			type scored struct {
				h sHit
				s float32
			}
			sv := make([]scored, len(hits))
			for i := range hits {
				sv[i] = scored{hits[i], scores[i]}
			}
			sort.Slice(sv, func(a, b int) bool { return sv[a].s > sv[b].s })
			for i := range hits {
				hits[i] = sv[i].h
			}
		}()
	}

	// Trim to 8 after optional reranking.
	if len(hits) > 8 {
		hits = hits[:8]
	}
	return hits
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
