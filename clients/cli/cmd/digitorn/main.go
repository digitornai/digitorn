// Command digitorn is the official TUI / CLI client for digitorn agent
// apps. It is a strict client : it ONLY consumes the public REST +
// Socket.IO contract of the daemon, it has ZERO Go-level coupling
// with the daemon module — they live in separate go modules to make
// that boundary enforce at compile time, not at code-review time.
//
// Run `digitorn --help` for the full command list. The TUI launches
// with `digitorn chat <app-id>`.

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/digitornai/digitorn-cli/internal/commands"
)

// version is set at build time via -ldflags "-X main.version=..."
// Default "dev" so unrelased binaries are obvious.
var version = "dev"

func main() {
	if err := fang.Execute(context.Background(), newRoot()); err != nil {
		// fang renders its own error display ; we just exit with the
		// right code. fang.Execute already prints the error in a
		// styled box.
		_ = err
		os.Exit(1)
	}
}



func newRoot() *cobra.Command {
		// Propagate build-time version to the commands package.
		commands.Version = version

		root := &cobra.Command{
			Use:   "digitorn",
			Short: "Official CLI client for the digitorn daemon",
			Long: "Digitorn is the terminal client for the digitorn daemon.\n" +
				"It talks to a running daemon over REST + Socket.IO and gives\n" +
				"you chat, app management, session browsing — all from your shell.",
			Version:       version,
			SilenceUsage:  true,
			SilenceErrors: true,
			RunE: func(cmd *cobra.Command, args []string) error {

				return cmd.Help()
			},
		}

		root.AddCommand(commands.NewChat())      // TUI chat (launches the opencode fork)
		root.AddCommand(commands.NewList())      // list installed apps
		root.AddCommand(commands.NewSessions())  // list sessions per app
		root.AddCommand(commands.NewInstall())   // install an app
		root.AddCommand(commands.NewUninstall()) // remove an app
		root.AddCommand(commands.NewEnable())    // enable an app
		root.AddCommand(commands.NewDisable())   // disable an app
		root.AddCommand(commands.NewLogin())     // OAuth sign-in via browser
		root.AddCommand(commands.NewLogout())    // wipe local credentials
		root.AddCommand(commands.NewWhoami())    // who's signed in
		root.AddCommand(commands.NewAppInfo())   // app info
		root.AddCommand(commands.NewAppStatus()) // app health
		root.AddCommand(commands.NewAppReload()) // app reload
		root.AddCommand(commands.NewDaemonStats()) // daemon stats
		root.AddCommand(commands.NewSecret())    // secret group
		root.AddCommand(commands.NewUpgrade())    // self-update
		root.AddCommand(commands.NewVersion())   // version
		root.AddCommand(commands.NewStatus())    // daemon status
		root.AddCommand(commands.NewDoctor())    // environment doctor
		return root
}

// ensureBuildable is a no-op guard called from tests to verify the
// command tree builds without panic. Real cmd_test.go in CLI-1.
var ensureBuildable = func() {
	_ = newRoot()
	_ = fmt.Sprintf("digitorn %s", version)
}
