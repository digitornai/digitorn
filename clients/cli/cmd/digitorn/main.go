package main

import (
	"context"
	"fmt"
	"os"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/digitornai/digitorn-cli/internal/commands"
)

var version = "dev"

func main() {
	if err := fang.Execute(context.Background(), newRoot()); err != nil {
		_ = err
		os.Exit(1)
	}
}



func newRoot() *cobra.Command {
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

		root.AddCommand(commands.NewChat())
		root.AddCommand(commands.NewList())
		root.AddCommand(commands.NewSessions())
		root.AddCommand(commands.NewInstall())
		root.AddCommand(commands.NewUninstall())
		root.AddCommand(commands.NewEnable())
		root.AddCommand(commands.NewDisable())
		root.AddCommand(commands.NewLogin())
		root.AddCommand(commands.NewLogout())
		root.AddCommand(commands.NewWhoami())
		root.AddCommand(commands.NewAppInfo())
		root.AddCommand(commands.NewAppStatus())
		root.AddCommand(commands.NewAppReload())
		root.AddCommand(commands.NewDaemonStats())
		root.AddCommand(commands.NewSecret())
		root.AddCommand(commands.NewUpgrade())
		root.AddCommand(commands.NewVersion())
		root.AddCommand(commands.NewStatus())
		root.AddCommand(commands.NewDoctor())
		return root
}

var ensureBuildable = func() {
	_ = newRoot()
	_ = fmt.Sprintf("digitorn %s", version)
}
