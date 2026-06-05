package bash

import (
	"fmt"
	"regexp"

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

var hardDenied = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*-[a-z]*r[a-z]*f?[a-z]*\s+/(\s|$|\*)`),
	regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*-[a-z]*f[a-z]*r?[a-z]*\s+/(\s|$|\*)`),
	regexp.MustCompile(`:\s*\(\s*\)\s*\{`),
	regexp.MustCompile(`(?i)\bmkfs\b`),
	regexp.MustCompile(`(?i)\bdd\b.*\bof=/dev/(sd|hd|nvme|disk)`),
	regexp.MustCompile(`(?i)>\s*/dev/(sd|hd|nvme|disk)`),
}

// checkCommand is an advisory last line of defense only. The real barrier is
// workspace confinement (the shell starts inside the workspace) plus the env
// allowlist. It refuses a small set of unambiguously destructive patterns; it
// is NOT a sandbox and makes no completeness claim.
func checkCommand(command string) error {
	for _, re := range hardDenied {
		if re.MatchString(command) {
			return fmt.Errorf("command refused by safety guard (matched a destructive pattern)")
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
