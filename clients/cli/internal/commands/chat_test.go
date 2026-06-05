package commands

import (
	"strings"
	"testing"
)

// terminalResetSeq must disable EVERY escape-mode a TUI can leave on, so no
// garbage leaks back to the shell after an unclean exit. This locks in the
// hard-won set — notably 1015 (urxvt mouse), whose omission was the Windows
// "[d;d;dM" leak that disabling 1006 alone never stopped.
func TestTerminalResetSeq_DisablesEveryLeakMode(t *testing.T) {
	required := map[string]string{
		"\x1b[?1000l": "normal mouse tracking",
		"\x1b[?1002l": "button-event mouse",
		"\x1b[?1003l": "any-motion mouse",
		"\x1b[?1006l": "SGR extended mouse",
		"\x1b[?1015l": "urxvt extended mouse (the Windows leak)",
		"\x1b[?1004l": "focus reporting",
		"\x1b[?2004l": "bracketed paste",
		"\x1b[?25h":   "show cursor",
		"\x1b[?1049l": "leave alt-screen",
	}
	for seq, what := range required {
		if !strings.Contains(terminalResetSeq, seq) {
			t.Errorf("terminalResetSeq missing %q (%s)", seq, what)
		}
	}
}
