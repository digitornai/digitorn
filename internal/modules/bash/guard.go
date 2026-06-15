package bash

import (
	"fmt"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/mbathepaul/digitorn/internal/modules/bash/goshell"
)

// backgroundAmpHint rejects a command whose last statement is a trailing `&`. A
// trailing `&` detaches the process from the daemon as an UNTRACKED orphan: no
// task_id, no captured logs, no signal when it dies, and it keeps holding its
// port — so a server "started" this way silently leaks and the agent is never
// told it crashed. The managed path (background_run) is the correct channel and
// gives all of that back. Returns "" when the command is fine.
func backgroundAmpHint(command string) string {
	if !goshell.TrailingBackground(command) {
		return ""
	}
	return "don't background with a trailing `&` — it detaches the process as an untracked orphan: no task_id, no captured logs, no notification when it dies, and it keeps holding its port (so the next launch hits EADDRINUSE). To run a server or any long-living command in the background, call `background_run` with the command WITHOUT the `&`: you get a task_id, a start-up check that surfaces an immediate crash, captured output, and a notification when it finishes or fails."
}

// foregroundServerPatterns match commands that NEVER return on their own — dev
// servers, preview servers, watchers. Run in the foreground they pin the turn
// until the timeout (default 900s) and then report a bogus failure when the kill
// finally lands; the cardinal rule is the loop is never blocked. They are
// matched on a QUOTE-MASKED copy (so `echo "npm run dev"` / `bash -c "vite"` are
// not flagged) and are deliberately conservative: nothing here matches a command
// that terminates on its own (build / test / install / lint / tsc), so a false
// positive never breaks a normal foreground step. `vite build` etc. stay allowed.
var foregroundServerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(npm|pnpm|yarn|bun)\s+(run\s+)?(dev|start|serve|preview|watch)\b`),
	regexp.MustCompile(`(?i)\bvite\s+(dev|serve|preview)\b`),
	regexp.MustCompile(`(?i)\bvite\s*($|[;&|])`), // bare `vite` / `npx vite` (defaults to the dev server)
	regexp.MustCompile(`(?i)\b(next|nuxt|remix)\s+(dev|start)\b`),
	regexp.MustCompile(`(?i)\bng\s+serve\b`),
	regexp.MustCompile(`(?i)\b(vue-cli-service|react-scripts)\s+(serve|start)\b`),
	regexp.MustCompile(`(?i)\bwebpack(-dev-server)?\s+serve\b`),
	regexp.MustCompile(`(?i)\bwebpack-dev-server\b`),
	regexp.MustCompile(`(?i)\bnodemon\b`),
	regexp.MustCompile(`(?i)\bflask\b.*\brun\b`),
	regexp.MustCompile(`(?i)\b(uvicorn|gunicorn|hypercorn|daphne|waitress-serve)\b`),
	regexp.MustCompile(`(?i)\bpython[0-9.]*\s+-m\s+http\.server\b`),
	regexp.MustCompile(`(?i)\bphp\s+-S\b`),
	regexp.MustCompile(`(?i)\brails\s+(server|s)\b`),
	regexp.MustCompile(`(?i)\b(http-server|live-server|serve)\b`),
	regexp.MustCompile(`(?i)\btail\s+-f\b`),
}

// foregroundServerHint rejects a long-running server / watcher run in the
// FOREGROUND (the caller skips this check when the dispatch is already a
// background_run). It returns "" for anything that isn't an unambiguous server.
// The message routes the agent to background_run AND states the fact that
// settles most of these calls: previewing the workspace needs no server at all —
// the build output is served automatically.
func foregroundServerHint(command string) string {
	masked := maskQuoted(command)
	for _, re := range foregroundServerPatterns {
		if re.MatchString(masked) {
			return "this looks like a long-running dev/preview server or watcher — it never returns on its own, so running it in the foreground freezes the turn until the timeout. Start it with `background_run` (no trailing `&`): you get a task_id, a startup check that surfaces an immediate crash, captured logs, and a notification when it exits. NOTE: you do NOT need to run a server to preview the app — the workspace serves the build output automatically. Just produce a production build (e.g. `npm run build`) and the preview attaches to it."
		}
	}
	return ""
}

// dosCommandEquivalents maps Windows cmd.exe commands that DON'T exist on a
// bash/sh shell to their bash equivalent. The model, seeing a Windows host,
// sometimes reaches for cmd syntax (`dir`, `copy`, `del`) and gets a cryptic
// "executable file not found" (exit 127) it then loops on. `dir` is included
// even though some Linux coreutils ship it — the Windows Git-Bash this targets
// has no `dir`, and a `dir` with cmd switches (`/B /S`) fails everywhere anyway.
// `type` is deliberately ABSENT: it is a real bash builtin (a legitimate call),
// so flagging it would block valid use.
var dosCommandEquivalents = map[string]string{
	"dir":     "ls (use `ls -R` to recurse, or `find . -type f` for a flat list)",
	"cls":     "clear",
	"copy":    "cp",
	"xcopy":   "cp -r",
	"del":     "rm",
	"erase":   "rm",
	"move":    "mv",
	"ren":     "mv",
	"rename":  "mv",
	"findstr": "grep",
}

// dosStatementSplit splits a command line into its top-level statements so the
// command WORD of each is checked (catches `cd app && dir /B /S`, where the bare
// command word is `cd`).
var dosStatementSplit = regexp.MustCompile(`[;&|]+`)

// dosCmdSwitch matches a cmd.exe-style single-letter switch token (`/B`, `/S`,
// `/Q`). Bash never uses these (flags are `-x`), and a one-letter `/X` is not a
// real path, so it is an unambiguous "the model thinks this is cmd.exe" signal.
var dosCmdSwitch = regexp.MustCompile(`(^|\s)/[A-Za-z](\s|$)`)

// dosHint returns a clear, actionable message when a command uses Windows
// cmd.exe syntax on a bash/sh shell (the caller skips it for a PowerShell
// target, which aliases dir/copy/del and has its own bashism translation).
// Runs on a quote-masked copy so a quoted literal (`echo "dir /B"`) is ignored.
func dosHint(command string) string {
	masked := maskQuoted(command)
	for _, seg := range dosStatementSplit.Split(masked, -1) {
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		if eq, ok := dosCommandEquivalents[strings.ToLower(fields[0])]; ok {
			return fmt.Sprintf("`%s` is a Windows cmd command — this is a bash shell, where it isn't available. Use `%s` instead. (Windows cmd commands like dir/copy/del/move/ren/cls have bash equivalents: ls/cp/rm/mv/mv/clear.)", strings.ToLower(fields[0]), eq)
		}
	}
	if dosCmdSwitch.MatchString(masked) {
		return "this is a bash shell, not Windows cmd.exe — `/B`, `/S`, `/Q`-style switches don't work here (bash flags are `-x`). e.g. `dir /B /S` → `find . -type f` (or `ls -R`)."
	}
	return ""
}

// hardDeniedFallback is the regex-only last-resort check, used when the AST
// parser cannot understand the input. The AST checker is always tried first
// and is authoritative when it succeeds — it has accurate command/arg
// boundaries, so it cannot be fooled by quoted flags or by `echo`-of-rm-
// looking text. The regex only fires for syntactically degenerate inputs that
// the parser rejects (e.g. a fork bomb whose `:(){…}` is not parseable as a
// CallExpr).
var hardDeniedFallback = []*regexp.Regexp{
	regexp.MustCompile(`:\s*\(\s*\)\s*\{`), // fork bomb: function ":" with `:|:&` body
	regexp.MustCompile(`(?i)>\s*/dev/(sd|hd|nvme|disk|vd|xvd|mmcblk|loop|mem|kmem|ram|fd)`),
}

// dangerousDevicePrefixes are device-name prefixes whose underlying block /
// kernel devices a command MUST NOT overwrite. Union of common storage names
// (sd, hd, nvme), macOS disks (disk), virtio-blk on KVM (vd), Xen / AWS EC2
// (xvd), eMMC / SD cards on ARM boards (mmcblk), loop devices (loop), kernel
// memory and main memory (mem, kmem, ram), and floppies (fd).
var dangerousDevicePrefixes = []string{
	"sd", "hd", "nvme", "disk", "vd", "xvd", "mmcblk", "loop",
	"mem", "kmem", "ram", "fd",
}

// checkCommand is an advisory last line of defense. The real barrier is
// workspace confinement plus the env allowlist; this layer refuses a small
// set of unambiguously destructive shapes. The AST-based checker understands
// shell quoting, long flags and command vs. literal boundaries — so it
// cannot be fooled by `rm "-rf" /` (quoted flag) or trigger a false positive
// on `echo "rm -rf /"` (echo, not rm).
func checkCommand(command string) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	file, perr := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if perr == nil {
		return walkCheckDestructive(file)
	}
	// AST parse failed → fall back to regex on the raw text.
	for _, re := range hardDeniedFallback {
		if re.MatchString(command) {
			return fmt.Errorf("command refused by safety guard (matched a destructive pattern)")
		}
	}
	return nil
}

// walkCheckDestructive walks every CallExpr in the file (so nested commands
// inside subshells, pipes, &&-chains and command substitutions are all
// inspected), and refuses on the first destructive shape it sees.
func walkCheckDestructive(file *syntax.File) error {
	var refusal error
	// Always check fork-bomb shape on the file's first statement structure —
	// the parser turns `:(){:|:&};:` into a function declaration + recursion,
	// which the per-call check below can't see.
	if forkBombShape(file) {
		return fmt.Errorf("command refused by safety guard (fork bomb)")
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		if refusal != nil {
			return false
		}
		switch n := node.(type) {
		case *syntax.CallExpr:
			if len(n.Args) > 0 {
				if err := checkCallExpr(n); err != nil {
					refusal = err
					return false
				}
			}
		case *syntax.Stmt:
			if err := checkRedirs(n.Redirs); err != nil {
				refusal = err
				return false
			}
		}
		return true
	})
	return refusal
}

// checkRedirs refuses output redirections that overwrite or append to a
// dangerous device (`> /dev/sda`, `>> /dev/nvme0`). The target is taken
// dequoted so `> "/dev/sda"` is caught too.
func checkRedirs(redirs []*syntax.Redirect) error {
	for _, r := range redirs {
		if r == nil || r.Word == nil {
			continue
		}
		op := r.Op
		if op != syntax.RdrOut && op != syntax.AppOut && op != syntax.RdrClob {
			continue
		}
		target := strings.ToLower(dequote(r.Word))
		const devPrefix = "/dev/"
		if !strings.HasPrefix(target, devPrefix) {
			continue
		}
		dev := target[len(devPrefix):]
		for _, p := range dangerousDevicePrefixes {
			if strings.HasPrefix(dev, p) {
				return fmt.Errorf("command refused by safety guard (redirect to block device %q)", target)
			}
		}
	}
	return nil
}

// forkBombShape spots the classic `:(){ :|:& };:` pattern: a function whose
// body calls itself recursively in a backgrounded pipe. Detection is structural
// (function name == ":" or any single char, body contains a call to itself
// piped and backgrounded) so quoting tricks cannot hide it.
func forkBombShape(file *syntax.File) bool {
	for _, stmt := range file.Stmts {
		fn, ok := stmt.Cmd.(*syntax.FuncDecl)
		if !ok {
			continue
		}
		name := fn.Name.Value
		if len(name) > 2 { // genuine fns have names; fork bombs use cryptic single chars
			continue
		}
		// Body containing both `|` (pipe) and `&` (background) calling the fn.
		var body strings.Builder
		_ = syntax.NewPrinter().Print(&body, fn.Body)
		text := body.String()
		if strings.Contains(text, "|") && strings.Contains(text, "&") && strings.Contains(text, name) {
			return true
		}
	}
	return false
}

func checkCallExpr(call *syntax.CallExpr) error {
	words := make([]string, 0, len(call.Args))
	for _, arg := range call.Args {
		words = append(words, dequote(arg))
	}
	cmd := strings.ToLower(commandBase(words[0]))
	args := words[1:]
	switch {
	case cmd == "rm":
		return checkRmArgs(args)
	case cmd == "dd":
		return checkDdArgs(args)
	case strings.HasPrefix(cmd, "mkfs"):
		// `mkfs`, `mkfs.ext4`, `mkfs.xfs`, … all format a device.
		return fmt.Errorf("command refused by safety guard (mkfs is destructive)")
	}
	return nil
}

// commandBase returns the basename of an invocation path, so `/bin/rm` and
// `rm` and `\rm` (the backslash form used to bypass aliases) all resolve to
// `rm`. Bash also accepts a leading `\` so we strip it before splitting.
func commandBase(p string) string {
	p = strings.TrimPrefix(p, `\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// dequote returns the string value of a parsed word, with single / double
// quotes removed exactly the way bash treats them at expansion: `"-rf"` and
// `'-rf'` both dequote to `-rf`. Complex words (parameter expansion, command
// substitution, globs) fall back to the printer's verbatim text so the check
// still has SOMETHING to match against.
func dequote(w *syntax.Word) string {
	var b strings.Builder
	complex := false
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, dq := range p.Parts {
				if lit, ok := dq.(*syntax.Lit); ok {
					b.WriteString(lit.Value)
				} else {
					complex = true
				}
			}
		default:
			complex = true
		}
	}
	if complex {
		var pb strings.Builder
		_ = syntax.NewPrinter().Print(&pb, w)
		return pb.String()
	}
	return b.String()
}

// checkRmArgs refuses `rm` invocations that combine "-r" (any spelling),
// "-f" (any spelling), AND a root-targeting path. All three must be present —
// `rm -rf ./build` keeps working, `rm -r ./testdata` keeps working, only the
// root-deletion shape is refused.
func checkRmArgs(args []string) error {
	hasRecurse, hasForce, dangerousPath := false, false, false
	for _, a := range args {
		switch a {
		case "--recursive", "-R":
			hasRecurse = true
			continue
		case "--force":
			hasForce = true
			continue
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(a) > 1 {
			flags := strings.ToLower(a[1:])
			if strings.Contains(flags, "r") {
				hasRecurse = true
			}
			if strings.Contains(flags, "f") {
				hasForce = true
			}
			continue
		}
		if isRootishPath(a) {
			dangerousPath = true
		}
	}
	if hasRecurse && hasForce && dangerousPath {
		return fmt.Errorf("command refused by safety guard (rm -r -f targeting filesystem root)")
	}
	return nil
}

// isRootishPath identifies POSIX root path arguments (`/`, `/*`, `/.`, `/.*`)
// and the bare-slash forms bash globbing expands into. NOT triggered by
// `/etc/passwd` or any other deeper path: the user explicitly wants to be
// able to delete inside the filesystem.
func isRootishPath(a string) bool {
	switch a {
	case "/", "/*", "/.", "/.*", "//", "/**":
		return true
	}
	return false
}

// checkDdArgs refuses dd writes to any block/kernel device whose prefix is
// in dangerousDevicePrefixes. Allowed: writes to plain files (e.g. an ISO
// build), and writes to harmless special files like `/dev/null` (which
// matches none of the dangerous prefixes).
func checkDdArgs(args []string) error {
	for _, a := range args {
		const ofPrefix = "of="
		if !strings.HasPrefix(a, ofPrefix) {
			continue
		}
		target := strings.ToLower(a[len(ofPrefix):])
		const devPrefix = "/dev/"
		if !strings.HasPrefix(target, devPrefix) {
			continue
		}
		dev := target[len(devPrefix):]
		for _, p := range dangerousDevicePrefixes {
			if strings.HasPrefix(dev, p) {
				return fmt.Errorf("command refused by safety guard (dd write to block device %q)", target)
			}
		}
	}
	return nil
}

// bashismPatterns match UNAMBIGUOUS bash syntax that PowerShell never produces,
// so detecting them can't false-positive on a real PowerShell command. Checked
// AFTER env-var translation (psEnv), so a translated `export`/inline-env no
// longer trips them — only genuinely un-runnable bash does.
var bashismPatterns = []*regexp.Regexp{
	regexp.MustCompile(`;\s*(then|do|done|fi|elif|esac)\b`),     // if/for/while/case bodies
	regexp.MustCompile(`\[\[?\s+-[a-zA-Z]\b`),                   // [ -f x ] / [[ -d y ]] test
	regexp.MustCompile(`(^|[;&|]|\bdo\b)\s*test\s+-[a-zA-Z]\b`), // test -f / test -d
	regexp.MustCompile(`(^|[;&|])\s*source\s`),                  // source venv/bin/activate
	regexp.MustCompile(`(^|[;&|])\s*export\s`),                  // export left untranslated ($-value)
}

// bashismHint returns a clear, actionable message when the command uses bash
// syntax that does NOT run on the PowerShell shell — so the agent gets guidance
// instead of a cryptic "[scriptblock]::Create" parse exception it would loop on.
// Returns "" when nothing unambiguously-bash is present.
//
// The check runs on a QUOTE-MASKED copy : bash syntax inside quotes is
// intentional (`bash -c "for x; do …; done"`, `echo "test -f …"`) and must NOT
// be flagged — explicitly invoking bash is the correct escape hatch, not an error.
func bashismHint(command string) string {
	masked := maskQuoted(command)
	for _, re := range bashismPatterns {
		if re.MatchString(masked) {
			return "this shell is PowerShell, not bash — that command uses bash-only syntax " +
				"(`if/for/while/[ ]/test`, `export`, or `source`) that won't run directly here. Either use " +
				"PowerShell (`$env:VAR='v'`, `foreach ($x in …) {…}`, `if (…) {…}`, `Test-Path`), or run the " +
				"exact bash via `bash -c '…'` — real bash is available; use SINGLE quotes so PowerShell does " +
				"not expand `$vars` before bash sees them — or write a script file with filesystem.write and " +
				"run it. Plain commands, pipes, `&&`, `||`, `$(…)`, `export VAR=v` and `2>/dev/null` already work."
		}
	}
	return ""
}

// maskQuoted blanks the contents of single/double-quoted spans (keeping the
// quote chars and overall length) so pattern matching ignores quoted text.
func maskQuoted(s string) string {
	b := []byte(s)
	var quote byte
	for i := 0; i < len(b); i++ {
		c := b[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				b[i] = ' '
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
		}
	}
	return string(b)
}
