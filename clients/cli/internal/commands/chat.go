package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/mbathepaul/digitorn-cli/internal/client"
	"github.com/mbathepaul/digitorn-cli/internal/theme"
	"github.com/mbathepaul/digitorn-cli/internal/tui"
)

// NewChat returns the `digitorn chat <app-id>` cobra command : opens
// the fullscreen TUI bound to that app. Up through CLI-2 the screen
// is blank with just the status bar ; CLI-3 plugs the chat input
// and the message history.
func NewChat() *cobra.Command {
	var sessionID string
	var workdir string
	cmd := &cobra.Command{
		Use:   "chat [app-id]",
		Short: "Open the TUI to chat with an app",
		Long: "Open the digitorn TUI for the given app. Omit the app id to " +
			"pick one from a list. Pass --session <sid> to resume an existing " +
			"session (use `digitorn sessions <app-id>` to find one). Without " +
			"--session, a fresh session is created in the current directory " +
			"(override with --workdir). Use Ctrl+Q to quit.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			appID := ""
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

// resolveWorkdir turns the --workdir flag into the absolute path the daemon
// expects. Empty flag → the directory the CLI was launched from, so the agent
// works on the project you're standing in. A relative flag is made absolute.
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
			return fmt.Errorf("%w\n\nIs the daemon running ? Try : digitornd -config bin/config-live-chat.yaml", err)
		}
		return err
	}

	c, err := client.New(clientOptions(url, creds))
	if err != nil {
		return err
	}

	// No app given : let the user pick one from the installed list. One
	// app → skip the picker ; none → a clear error.
	if appID == "" {
		apps, err := c.ListApps(ctx, false)
		if err != nil {
			return fmt.Errorf("list apps : %w", err)
		}
		switch len(apps) {
		case 0:
			return fmt.Errorf("no apps installed (try : digitorn list)")
		case 1:
			appID = apps[0].AppID
		default:
			appID, err = tui.SelectApp(apps, theme.Preferred())
			if err != nil {
				return fmt.Errorf("select app : %w", err)
			}
			if appID == "" {
				return nil // cancelled
			}
		}
	}

	// Fetch the app metadata up front so the status bar shows the
	// pretty name + model. Hard fail if the app isn't installed —
	// the user shouldn't be staring at an empty TUI wondering why
	// nothing works.
	app, err := c.GetApp(ctx, appID)
	if err != nil {
		var apiErr *client.APIError
		if asAPIErr(err, &apiErr) && apiErr.StatusCode == 404 {
			return fmt.Errorf("app %q is not installed (try : digitorn list)", appID)
		}
		return fmt.Errorf("fetch app %q : %w", appID, err)
	}

	model := tui.New(tui.Options{
		Client:          c,
		Theme:           theme.Preferred(),
		AppID:           appID,
		AppName:         app.Name,
		Credentials:     creds,
		ResumeSessionID: resumeSessionID,
		Workdir:         workdir,
	})

	// Bubble Tea v2 enables alt-screen + mouse via Msg sent from
	// Init() ; no more WithAltScreen / WithMouseCellMotion options.
	prog := tea.NewProgram(model)
	// Bubble Tea fully restores the terminal on EVERY exit it controls — clean
	// quit, Ctrl+C/SIGINT, and panic (its renderer teardown disables mouse modes
	// 1002/1003/1006, shows the cursor, leaves alt-screen). The ONLY way mouse
	// tracking leaks (every mouse move then echoes "[<b;x;y>M" garbage to the
	// shell) is a FORCED kill of this process — `Stop-Process -Force` / taskkill
	// /F / closing the window abruptly — which bypasses tea's shutdown AND Go
	// defers. Nothing in-process can clean up after TerminateProcess, so we make
	// it SELF-HEALING instead :
	//   - resetTerminal() at startup clears any mouse mode a previous force-killed
	//     run left on, so simply relaunching the CLI fixes a dirty terminal ;
	//   - the deferred resetTerminal() is a redundant backup for clean exits.
	// To AVOID the leak in the first place : quit with `q` / Ctrl+C / Esc — don't
	// kill the process or close the window. When rebuilding, quit the CLI first.
	resetTerminal()
	defer resetTerminal()
	if _, err := prog.Run(); err != nil {
		return fmt.Errorf("tui : %w", err)
	}
	return nil
}

// resetTerminal undoes EVERYTHING a TUI can leave behind after an unclean exit,
// so no escape-sequence leak survives back to the shell. Per the Windows fix in
// kilo/opencode (disable ALL mouse modes incl. 1015, plus the Kitty keyboard
// protocol, on every exit path) it disables :
//   - mouse reporting : 1000 (normal), 1002 (button), 1003 (any-motion),
//     1006 (SGR ext), 1015 (urxvt ext — the "[d;d;dM" format with NO '<', the
//     exact garbage seen on Windows ; disabling 1006 alone does NOT stop it) ;
//   - 1004 focus reporting (the "[I"/"[O" leak), 2004 bracketed paste ;
//   - the Kitty keyboard protocol (CSI <u pops it) + modifyOtherKeys (CSI >4m) ;
// then shows the cursor (25h) and leaves the alt-screen (1049l). All idempotent,
// so it's safe as a startup self-heal AND the supervisor's on-exit wipe.
func resetTerminal() {
	fmt.Fprint(os.Stdout, terminalResetSeq)
}

// terminalResetSeq is the exact wipe resetTerminal emits. Extracted as a const
// so a regression test can assert every leak-causing mode stays disabled — in
// particular 1015 (urxvt mouse), whose omission was the Windows leak that
// disabling 1006 alone never fixed.
const terminalResetSeq = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1015l\x1b[?1004l\x1b[?2004l\x1b[<u\x1b[>4m\x1b[?25h\x1b[?1049l"

// asAPIErr is a thin wrapper around errors.As that doesn't require
// the caller to import the errors package just for this check.
func asAPIErr(err error, target **client.APIError) bool {
	for cur := err; cur != nil; {
		if a, ok := cur.(*client.APIError); ok {
			*target = a
			return true
		}
		type wrapped interface{ Unwrap() error }
		if w, ok := cur.(wrapped); ok {
			cur = w.Unwrap()
			continue
		}
		break
	}
	return false
}
