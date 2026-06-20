package commands

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbathepaul/digitorn-cli/internal/client"
)

// Version is the CLI version, set at build time via -ldflags.
var Version = "dev"

// NewVersion returns `digitorn version`.
func NewVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show digitorn CLI version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "digitorn %s %s/%s\n", Version, runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

// NewStatus returns `digitorn status` — checks if the daemon is reachable.
func NewStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check if the daemon is running",
		Long:  "Ping the daemon's health endpoint to verify it's running and reachable.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url := client.Discover()
			c, err := client.New(client.Options{BaseURL: url})
			if err != nil {
				return fmt.Errorf("client init: %w", err)
			}
			ctx := cmd.Context()
			if err := c.Ping(ctx); err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "daemon NOT reachable at %s\n", url)
				fmt.Fprintf(cmd.OutOrStdout(), "Is it running? Try: digitornd -config config.yaml run\n")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "daemon is running at %s\n", url)
			return nil
		},
	}
}

// NewDoctor returns `digitorn doctor` — environment and daemon diagnostics.
func NewDoctor() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check system prerequisites and daemon health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			allOK := true

			fmt.Fprintln(out, "Digitorn Doctor")
			fmt.Fprintln(out, "---------------")

			// 1. CLI version
			fmt.Fprintf(out, "CLI:       digitorn %s %s/%s\n", Version, runtime.GOOS, runtime.GOARCH)

			// 2. Daemon reachability
			url := client.Discover()
			c, err := client.New(client.Options{BaseURL: url})
			if err != nil {
				fmt.Fprintf(out, "Daemon:    %s\n", err)
				allOK = false
			} else if err := c.Ping(ctx); err != nil {
				fmt.Fprintf(out, "Daemon:    NOT REACHABLE at %s\n", url)
				fmt.Fprintf(out, "           Is it running? Try: digitornd -config config.yaml run\n")
				allOK = false
			} else {
				fmt.Fprintf(out, "Daemon:    running at %s\n", url)
				sc, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				stats, err := c.DaemonStats(sc)
				if err == nil {
					if id, _ := stats["instance_id"].(string); id != "" {
						fmt.Fprintf(out, "Instance:  %s\n", id)
					}
					if up, _ := stats["uptime_secs"].(float64); up > 0 {
						d := time.Duration(up) * time.Second
						fmt.Fprintf(out, "Uptime:    %v\n", d.Round(time.Second))
					}
				}
			}

			fmt.Fprintln(out, "---------------")
			if allOK {
				fmt.Fprintln(out, "All checks passed.")
			} else {
				fmt.Fprintln(out, "Some checks failed — see above.")
			}
			return nil
		},
	}
}
