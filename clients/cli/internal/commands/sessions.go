package commands

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/mbathepaul/digitorn-cli/internal/client"
)

// NewSessions builds `digitorn sessions <app-id>` : a table of the
// sessions stored under that app, with seq counts + last update times.
// Used to discover a session_id that can then be passed to
// `digitorn chat <app-id> --session <sid>` for resume.
func NewSessions() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "sessions <app-id>",
		Short: "List sessions of an installed app",
		Long: "Print every session the daemon has for the given app, sorted " +
			"by last-updated descending. The session_id can be passed to " +
			"`digitorn chat <app-id> --session <sid>` to resume.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessions(cmd.Context(), cmd.OutOrStdout(), args[0], limit)
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "max sessions to display (newest first)")
	return cmd
}

func runSessions(ctx context.Context, out io.Writer, appID string, limit int) error {
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
			return fmt.Errorf("%w\n\nIs the daemon running ?", err)
		}
		return err
	}
	c, err := client.New(clientOptions(url, creds))
	if err != nil {
		return err
	}
	resp, err := c.ListSessions(ctx, appID, limit, 0)
	if err != nil {
		return fmt.Errorf("list sessions of %q : %w", appID, err)
	}
	if resp == nil || len(resp.Sessions) == 0 {
		fmt.Fprintln(out, "no sessions yet — start one with `digitorn chat "+appID+"`")
		return nil
	}
	renderSessionsTable(out, resp.Sessions)
	return nil
}

func renderSessionsTable(w io.Writer, sessions []client.Session) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
		hdrStyle.Render("SESSION_ID"),
		hdrStyle.Render("TITLE"),
		hdrStyle.Render("EVENTS"),
		hdrStyle.Render("LAST UPDATE"),
		hdrStyle.Render("STATUS"),
	)
	for _, s := range sessions {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
			s.SessionID,
			renderTitle(s.Title),
			s.EventCount,
			renderRelTime(s.UpdatedAt),
			renderStatus(s),
		)
	}
	_ = tw.Flush()
}

func renderTitle(t string) string {
	if t == "" {
		return mutedStyle.Render("(untitled)")
	}
	return t
}

func renderRelTime(s string) string {
	if s == "" {
		return mutedStyle.Render("—")
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, s); err != nil {
			return mutedStyle.Render(s)
		}
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return mutedStyle.Render("just now")
	case d < time.Hour:
		return mutedStyle.Render(fmt.Sprintf("%dm ago", int(d.Minutes())))
	case d < 24*time.Hour:
		return mutedStyle.Render(fmt.Sprintf("%dh ago", int(d.Hours())))
	default:
		return mutedStyle.Render(fmt.Sprintf("%dd ago", int(d.Hours()/24)))
	}
}

var (
	sessionClosedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	sessionLiveStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	sessionInterStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

func renderStatus(s client.Session) string {
	switch {
	case s.Interrupted:
		return sessionInterStyle.Render("interrupted")
	case s.Closed:
		return sessionClosedStyle.Render("closed")
	default:
		return sessionLiveStyle.Render("live")
	}
}
