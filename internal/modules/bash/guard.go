package bash

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"mvdan.cc/sh/v3/syntax"

	"github.com/mbathepaul/digitorn/internal/modules/bash/goshell"
)

func backgroundAmpHint(command string) string {
	if !goshell.TrailingBackground(command) {
		return ""
	}
	return "don't background with a trailing `&` — it detaches the process as an untracked orphan: no task_id, no captured logs, no notification when it dies, and it keeps holding its port (so the next launch hits EADDRINUSE). To run a server or any long-living command in the background, call `background_run` with the command WITHOUT the `&`: you get a task_id, a start-up check that surfaces an immediate crash, captured output, and a notification when it finishes or fails."
}

var foregroundServerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(npm|pnpm|yarn|bun)\s+(run\s+)?(dev|start|serve|preview|watch)\b`),
	regexp.MustCompile(`(?i)\bvite\s+(dev|serve|preview)\b`),
	regexp.MustCompile(`(?i)\bvite\s*($|[;&|])`),
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

func foregroundServerHint(command string) string {
	masked := maskQuoted(command)
	for _, re := range foregroundServerPatterns {
		if re.MatchString(masked) {
			return "this looks like a long-running dev/preview server or watcher — it never returns on its own, so running it in the foreground freezes the turn until the timeout. Start it with `background_run` (no trailing `&`): you get a task_id, a startup check that surfaces an immediate crash, captured logs, and a notification when it exits. NOTE: you do NOT need to run a server to preview the app — the workspace serves the build output automatically. Just produce a production build (e.g. `npm run build`) and the preview attaches to it."
		}
	}
	return ""
}

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

var dosStatementSplit = regexp.MustCompile(`[;&|]+`)

var dosCmdSwitch = regexp.MustCompile(`(^|\s)/[A-Za-z](\s|$)`)

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

var hardDeniedFallback = []*regexp.Regexp{
	regexp.MustCompile(`:\s*\(\s*\)\s*\{`),
	regexp.MustCompile(`(?i)>\s*/dev/(sd|hd|nvme|disk|vd|xvd|mmcblk|loop|mem|kmem|ram|fd)`),
}

var dangerousDevicePrefixes = []string{
	"sd", "hd", "nvme", "disk", "vd", "xvd", "mmcblk", "loop",
	"mem", "kmem", "ram", "fd",
}

func checkCommand(command string) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	file, perr := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if perr == nil {
		return walkCheckDestructive(file)
	}
	for _, re := range hardDeniedFallback {
		if re.MatchString(command) {
			return fmt.Errorf("command refused by safety guard (matched a destructive pattern)")
		}
	}
	return nil
}

func walkCheckDestructive(file *syntax.File) error {
	var refusal error
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

func forkBombShape(file *syntax.File) bool {
	for _, stmt := range file.Stmts {
		fn, ok := stmt.Cmd.(*syntax.FuncDecl)
		if !ok {
			continue
		}
		name := fn.Name.Value
		if len(name) > 2 {
			continue
		}
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
		return fmt.Errorf("command refused by safety guard (mkfs is destructive)")
	}
	return nil
}

func commandBase(p string) string {
	p = strings.TrimPrefix(p, `\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

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

func isRootishPath(a string) bool {
	switch a {
	case "/", "/*", "/.", "/.*", "//", "/**":
		return true
	}
	return false
}

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


func needsPTY(command string) bool {
	masked := maskQuoted(command)
	for _, re := range ptyAutoPatterns {
		if re.MatchString(masked) {
			return true
		}
	}
	return false
}

// promptGraceWindow is how long a suspected interactive prompt must sit at the
// tail of a still-running command's output — with NO further output — before we
// treat it as a genuine blocking prompt (and not a "password:" string that
// happened to be printed mid-stream). Combined with "must be the last non-empty
// line" + "process still alive", it makes a false positive very unlikely.
const promptGraceWindow = 2 * time.Second

// interactivePrompt matches the line a command prints when it stops to wait for
// human input on a terminal: a sudo/ssh password prompt, a yes/no confirmation,
// a "press enter" pause, etc. Anchored at end-of-string so it only fires on a
// TRAILING prompt (the last thing emitted), never on prompt-looking text that
// is followed by more output.
var interactivePrompt = regexp.MustCompile(`(?i)(` +
	`password(\s+for\s+\S+)?\s*:|` +
	`enter\s+passphrase[^:]*:|passphrase[^:]*:|` +
	`\[y/n\]\??|\(yes/no(/\[fingerprint\])?\)\??|\[sudo\]|` +
	`are you sure[^\n]*|press\s+(enter|return|any\s+key)[^\n]*|` +
	`authenticity of host[^\n]*|` +
	`continue\?|overwrite\?|proceed\?` +
	`)\s*$`)

// looksLikePrompt reports whether the LAST non-empty line of a command's output
// so far looks like an interactive prompt waiting for input. Only the final line
// matters: a real prompt is the last thing a blocked program prints.
func looksLikePrompt(output string) bool {
	if output == "" {
		return false
	}
	tail := strings.TrimRight(output, " \t\r\n")
	if i := strings.LastIndexAny(tail, "\r\n"); i >= 0 {
		tail = tail[i+1:]
	}
	if tail == "" {
		return false
	}
	return interactivePrompt.MatchString(tail)
}

var ptyAutoPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bdocker\s+(?:run|exec)\b[^|&;]*(?:\s-[a-zA-Z]*t|-{1,2}tty)\b`),

	regexp.MustCompile(`(?i)(?:^|[|;&]\s*)\bssh\b\s+\S`),

	regexp.MustCompile(`(?i)\bwinget\s+(?:install|upgrade|import)\b`),

	regexp.MustCompile(`(?i)\baz\s+login\b`),
	regexp.MustCompile(`(?i)\bgcloud\s+auth\s+login\b`),
	regexp.MustCompile(`(?i)\bgh\s+auth\s+login\b`),
	regexp.MustCompile(`(?i)\baws\s+(?:configure|sso\s+login)\b`),

	regexp.MustCompile(`(?i)\bsudo\s+(?:su\b|-[isSu]\b)`),
}

type bashismEntry struct {
	re  *regexp.Regexp
	msg string
}

var bashismChecks = []bashismEntry{
	{
		re: regexp.MustCompile(`(^|[;&|])\s*source\s`),
		msg: "PowerShell uses dot-sourcing instead of `source`: `. ./script.ps1`  " +
			"If the script is bash, run it via `bash -c '. ./script.sh && <next command>'` " +
			"(single quotes so PowerShell doesn't expand $vars before bash sees them).",
	},
	{
		re: regexp.MustCompile(`;\s*(then|do|done|fi|elif|esac)\b`),
		msg: "PowerShell uses different control-flow syntax — bash `if/for/while/case` " +
			"don't parse here.\n" +
			"  bash: `if [[ -f x ]]; then cmd; fi`\n" +
			"  PS:   `if (Test-Path x) { cmd }`\n" +
			"  bash: `for f in *.go; do echo $f; done`\n" +
			"  PS:   `foreach ($f in Get-ChildItem *.go) { Write-Output $f.Name }`\n" +
			"Alternatively, write a bash script with filesystem.write and run it via `bash -c`.",
	},
	{
		re: regexp.MustCompile(`\[\[?\s+-[a-zA-Z]\b`),
		msg: "Bash test syntax `[ -f x ]` / `[[ -d y ]]` is not PowerShell.\n" +
			"  `-f file`  →  `Test-Path file -PathType Leaf`\n" +
			"  `-d dir`   →  `Test-Path dir  -PathType Container`\n" +
			"  `-z $VAR`  →  `[string]::IsNullOrEmpty($env:VAR)`\n" +
			"  `$A == $B` →  `$A -eq $B`",
	},
	{
		re: regexp.MustCompile(`(^|[;&|]|\bdo\b)\s*test\s+-[a-zA-Z]\b`),
		msg: "Bash `test -f file` is not available in PowerShell. Use `Test-Path file -PathType Leaf` instead.",
	},
	{
		re: regexp.MustCompile(`(^|[;&|])\s*export\s`),
		msg: "Bare `export VAR=value` was not translated (complex value). " +
			"Use `$env:VAR = 'value'` in PowerShell, or pass it inline: `$env:VAR='v'; command`.",
	},
}

func bashismHint(command string) string {
	masked := maskQuoted(command)
	for _, e := range bashismChecks {
		if e.re.MatchString(masked) {
			return "PowerShell syntax required — " + e.msg +
				"\n\nAlways available: plain commands, pipes (`|`), " +
				"`&&`/`||` chaining, `$(cmd)` substitution, `$env:VAR`, `>/dev/null`, `2>&1`."
		}
	}
	return ""
}

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
