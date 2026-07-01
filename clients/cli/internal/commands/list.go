// Package commands holds the batch (non-TUI) cobra subcommands. Each
// command is one file, dependency-injected against client.Client so
// tests can swap a stub without spinning httptest.
package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/digitornai/digitorn-cli/internal/client"
)

// NewList returns the `digitorn list` command : prints every
// installed app as a colored table. Flags : --all to include
// disabled apps. Output is written to stdout (tabwriter-aligned)
// with lipgloss-styled badges for booleans.
func NewList() *cobra.Command {
	var includeDisabled bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed digitorn apps",
		Long:  "Show every app the daemon knows about. Use --all to include disabled apps.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd.Context(), cmd.OutOrStdout(), includeDisabled)
		},
	}
	cmd.Flags().BoolVarP(&includeDisabled, "all", "a", false, "include disabled apps")
	return cmd
}

func runList(ctx context.Context, out io.Writer, includeDisabled bool) error {
	creds, err := loadFreshCreds(ctx)
	if err != nil {
		return err
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

	apps, err := c.ListApps(ctx, includeDisabled)
	if err != nil {
		return fmt.Errorf("list apps : %w", err)
	}
	if len(apps) == 0 {
		fmt.Fprintln(out, "no apps installed (try : digitorn install <source>)")
		return nil
	}
	renderAppsTable(out, apps)
	return nil
}

func tokenFromCreds(c *client.Credentials) string {
	if c == nil {
		return ""
	}
	return c.AccessToken
}

// ---- table rendering ----------------------------------------------

// Pre-built lipgloss styles. Reused for every row so the cost is paid
// once at first call.
var (
	hdrStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	mutedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	enabledStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))  // green
	disabledStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // red
	byokOnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // orange
)

func renderAppsTable(w io.Writer, apps []client.App) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
		hdrStyle.Render("APP_ID"),
		hdrStyle.Render("NAME"),
		hdrStyle.Render("VERSION"),
		hdrStyle.Render("ENABLED"),
		hdrStyle.Render("BYOK"),
		hdrStyle.Render("CATEGORY"),
	)
	for _, a := range apps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			a.AppID,
			a.Name,
			a.Version,
			renderEnabled(a.Enabled),
			renderBYOK(a.BYOK),
			renderCategory(a.Category),
		)
	}
	_ = tw.Flush()
}

func renderEnabled(b bool) string {
	if b {
		return enabledStyle.Render("ON")
	}
	return disabledStyle.Render("OFF")
}

func renderBYOK(b bool) string {
	if b {
		return byokOnStyle.Render("YES")
	}
	return mutedStyle.Render("no")
}

func renderCategory(s string) string {
	if s == "" {
		return mutedStyle.Render("—")
	}
	return s
}

// FprintErrln writes to stderr with optional styling. Helper used by
// cmd entrypoints that want a uniform error appearance independent of
// fang's styled error box.
func FprintErrln(args ...any) {
	_, _ = fmt.Fprintln(os.Stderr, args...)
}
