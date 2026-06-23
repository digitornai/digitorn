package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mbathepaul/digitorn-cli/internal/client"
)

func NewChat() *cobra.Command {
	var sessionID string
	var workdir string
	cmd := &cobra.Command{
		Use:   "chat [app-id]",
		Short: "Open the TUI to chat with an app",
		Long: "Open the digitorn TUI for the given app. Defaults to digitorn-code " +
			"when no app id is given. Uses the digitorn-tui binary.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			appID := "digitorn-code"
			if len(args) > 0 {
				appID = args[0]
			}
			return runChat(cmd.Context(), appID, sessionID, resolveWorkdir(workdir))
		},
	}
	cmd.Flags().StringVarP(&sessionID, "session", "s", "", "resume an existing session by ID")
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "",
		"agent working directory for a new session (default: the current directory)")
	return cmd
}

func resolveWorkdir(flag string) string {
	if flag == "" {
		if cwd, err := os.Getwd(); err == nil {
			return cwd
		}
		return ""
	}
	if abs, err := filepath.Abs(flag); err == nil {
		return abs
	}
	return flag
}

// tuiBinary returns the TUI binary to launch.
// Priority:
//  1. "digitorn-tui" binary next to the digitorn CLI (production)
//  2. ../digitorn-tui repo sibling of digitorn (development, separate repo)
//  3. clients/opencode-fork inside digitorn (legacy monorepo layout)
func tuiBinary() (path string, isDir bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	root := filepath.Dir(exe)

	// 1. Shipped binary alongside digitorn CLI.
	bin := filepath.Join(root, "digitorn-tui")
	if fi, err := os.Stat(bin); err == nil && !fi.IsDir() {
		return bin, false
	}

	// 2. Separate digitorn-tui repo, sibling of the digitorn directory.
	if abs, err := filepath.Abs(filepath.Join(root, "..", "..", "..", "digitorn-tui")); err == nil {
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			return abs, true
		}
	}

	// 3. Legacy: opencode-fork still inside clients/ (monorepo layout).
	dev := filepath.Join(root, "..", "..", "..", "clients", "opencode-fork")
	if fi, err := os.Stat(dev); err == nil && fi.IsDir() {
		return dev, true
	}

	return "", false
}

func runChat(ctx context.Context, appID, resumeSessionID, workdir string) error {
	creds, err := loadFreshCreds(ctx)
	if err != nil {
		return err
	}
	if creds == nil || creds.AccessToken == "" {
		return fmt.Errorf("not signed in — run `digitorn login` first")
	}
	url, err := client.DiscoverAndPing(ctx, 0)
	if err != nil {
		if client.IsUnreachable(err) {
			return fmt.Errorf("%w\n\nIs the daemon running? Try: digitornd -config config.yaml run", err)
		}
		return err
	}
	tui, isDir := tuiBinary()
	if tui == "" {
		return fmt.Errorf("digitorn-tui not found — place the 'digitorn-tui' binary next to digitorn")
	}
	env := os.Environ()
	env = append(env, "DIGITORN_APP="+appID)
	env = append(env, "DIGITORN_URL="+url)
	env = append(env, "DIGITORN_GATEWAY_URL=https://gateway.digitorn.ai/v1")
	if workdir != "" {
		env = append(env, "DIGITORN_CWD="+workdir)
	}
	if resumeSessionID != "" {
		env = append(env, "DIGITORN_SESSION="+resumeSessionID)
	}
	fmt.Fprintf(os.Stderr, "▶ app: %s   daemon: %s\n", appID, url)
	var cmd *exec.Cmd
	if isDir {
		if _, err := exec.LookPath("bun"); err != nil {
			return fmt.Errorf("bun not found — install with: curl -fsSL https://bun.sh/install | bash")
		}
		cmd = exec.CommandContext(ctx, "bun", "run", "dev")
		cmd.Dir = tui
	} else {
		cmd = exec.CommandContext(ctx, tui)
	}
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("digitorn-tui: %w", err)
	}
	return nil
}
