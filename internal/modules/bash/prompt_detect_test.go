package bash

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLooksLikePrompt(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		expect bool
	}{
		{"sudo password", "[sudo] password for paul: ", true},
		{"plain password", "Password:", true},
		{"ssh passphrase", "Enter passphrase for key '/home/x/.ssh/id_rsa': ", true},
		{"ssh authenticity", "Are you sure you want to continue connecting (yes/no/[fingerprint])? ", true},
		{"yes no", "Do you want to continue? [y/N] ", true},
		{"press enter", "Press ENTER to continue", true},
		{"prompt mid stream then more output", "Password: \nAuthenticated\nDone", false},
		{"normal output", "build finished\nall tests passed", false},
		{"empty", "", false},
		{"word password in sentence", "the password was changed successfully", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikePrompt(c.out); got != c.expect {
				t.Fatalf("looksLikePrompt(%q) = %v, want %v", c.out, got, c.expect)
			}
		})
	}
}

// TestPTY_BlockingPromptDoesNotHang runs a command that prints a password
// prompt and then blocks on `read`. The watchdog must detect it and return
// WaitingForInput within a few seconds instead of hanging until the timeout.
func TestPTY_BlockingPromptDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash prompt scenario is POSIX-specific")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	// Print a prompt with no trailing newline, then block reading stdin.
	cmd := `printf 'Password: '; read -r answer; echo "SHOULD_NOT_REACH:$answer"`

	start := time.Now()
	res := runWithPTY(context.Background(), "bash", bash, cmd, t.TempDir(), nil,
		64*1024, 30*time.Second, 2*time.Second)
	elapsed := time.Since(start)

	if !res.WaitingForInput {
		t.Fatalf("expected WaitingForInput=true, got %+v (elapsed %s)", res, elapsed)
	}
	if elapsed > 12*time.Second {
		t.Fatalf("watchdog too slow: %s (should be a few seconds)", elapsed)
	}
	if strings.Contains(res.Stdout, "SHOULD_NOT_REACH") {
		t.Fatalf("command should have been killed before reading input, got stdout: %q", res.Stdout)
	}
}

// TestPTY_NormalCommandStillSucceeds guards against the watchdog killing a
// quick, non-interactive command.
func TestPTY_NormalCommandStillSucceeds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash scenario is POSIX-specific")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	res := runWithPTY(context.Background(), "bash", bash, "echo hello", t.TempDir(), nil,
		64*1024, 30*time.Second, 2*time.Second)
	if res.WaitingForInput || res.TimedOut || res.Cancelled || res.ExitCode != 0 {
		t.Fatalf("normal command misclassified: %+v", res)
	}
	if !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("expected output 'hello', got %q", res.Stdout)
	}
}
