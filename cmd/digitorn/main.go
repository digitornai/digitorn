// Command digitorn is the user-facing CLI for the Digitorn daemon: lint
// manifests, deploy apps, manage sessions, install services.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbathepaul/digitorn/internal/compiler"
	"github.com/mbathepaul/digitorn/internal/compiler/codegen"
	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/version"
)

func main() {
	root := &cobra.Command{
		Use:     "digitorn",
		Short:   "Digitorn CLI — manage agent apps and the local daemon.",
		Version: version.String(),
	}

	root.AddCommand(lintCmd())
	root.AddCommand(compileCmd())
	root.AddCommand(inspectCmd())
	root.AddCommand(versionCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func lintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lint <path>",
		Short: "Validate an app YAML manifest (powered by the javac-grade compiler)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := compiler.New()
			res, err := c.CompileFile(args[0])
			if err != nil {
				return err
			}
			diagnostic.FormatBag(os.Stderr, res.Diagnostics, diagnostic.DefaultFormatOptions())
			if !res.OK() {
				os.Exit(1)
			}
			fmt.Printf("OK: app %q (version %s) — %d agent(s)\n",
				res.Definition.App.AppID,
				res.Definition.App.Version,
				len(res.Definition.Agents),
			)
			return nil
		},
	}
}

func compileCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "compile <bundle>",
		Short: "Compile an app bundle to a .dgc artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := compiler.New()
			res, err := c.Compile(args[0])
			if err != nil {
				return err
			}
			diagnostic.FormatBag(os.Stderr, res.Diagnostics, diagnostic.DefaultFormatOptions())
			if !res.OK() {
				os.Exit(1)
			}
			art, err := c.Build(res)
			if err != nil {
				return err
			}
			if output == "" {
				output = res.Definition.App.AppID + ".dgc"
			}
			if err := codegen.WriteFile(output, art); err != nil {
				return err
			}
			fmt.Printf("compiled %s -> %s\n  app:    %s (v%s)\n  agents: %d\n  hash:   %s\n",
				args[0], output,
				res.Definition.App.AppID, res.Definition.App.Version,
				len(res.Definition.Agents),
				art.Header.VersionHash,
			)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output .dgc path (default: <app_id>.dgc)")
	return cmd
}

func inspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <file.dgc>",
		Short: "Inspect a compiled .dgc artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			art, err := codegen.ReadFile(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("%s\n", args[0])
			fmt.Printf("  magic:           %s\n", string(art.Header.Magic[:]))
			fmt.Printf("  format:          v%d\n", art.Header.Format)
			fmt.Printf("  compiler:        %s\n", art.Header.CompilerVersion)
			fmt.Printf("  compiled at:     %s\n", time.Unix(art.Header.CompiledAt, 0).UTC().Format(time.RFC3339))
			fmt.Printf("  version hash:    %s\n", art.Header.VersionHash)
			fmt.Printf("  app:             %s (v%s)\n", art.Definition.App.AppID, art.Definition.App.Version)
			fmt.Printf("  agents:          %d\n", len(art.Definition.Agents))
			for _, a := range art.Definition.Agents {
				fmt.Printf("    - %s (%s/%s)\n", a.ID, a.Brain.Provider, a.Brain.Model)
			}
			return nil
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("digitorn " + version.String())
		},
	}
}
