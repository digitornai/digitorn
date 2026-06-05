package commands

import (
	"context"
	"fmt"
	"os"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/mbathepaul/digitorn-cli/internal/client"
)

// NewLogin builds `digitorn login` — opens the browser at auth.digitorn.ai,
// listens locally for the callback, and stores the resulting JWT in
// ~/.digitorn/credentials.json. Mirrors the legacy Python daemon's flow
// 1:1 ; no JWT trimballed via env var anymore.
func NewLogin() *cobra.Command {
	var (
		issuerURL string
		provider  string
		force     bool
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to digitorn via your browser",
		Long: "Authenticate against auth.digitorn.ai. The CLI opens your " +
			"default browser to the upstream OAuth provider (Google by default), " +
			"the auth server bounces the resulting token back to a localhost " +
			"listener, and we save it to ~/.digitorn/credentials.json.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogin(cmd.Context(), issuerURL, provider, force)
		},
	}
	cmd.Flags().StringVar(&issuerURL, "auth-url", "", "auth server URL (default: env DIGITORN_AUTH_URL or https://auth.digitorn.ai)")
	cmd.Flags().StringVarP(&provider, "provider", "p", "google", "upstream OAuth provider : google | microsoft | azure")
	cmd.Flags().BoolVar(&force, "force", false, "force re-login even if already signed in")
	return cmd
}

// NewLogout builds `digitorn logout` — wipes the local credentials file.
// Server-side revocation is NOT performed (no standardized RFC 7009 across
// providers ; openauthjs handles session expiry on its end).
func NewLogout() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the local digitorn credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := client.DeleteCredentials(); err != nil {
				return err
			}
			fmt.Println("✓ logged out (local credentials removed)")
			return nil
		},
	}
}

// NewWhoami builds `digitorn whoami` — prints the email + user_id from the
// stored credentials, or a clear "not signed in" message.
func NewWhoami() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the currently signed-in account",
		RunE: func(cmd *cobra.Command, args []string) error {
			creds, err := client.LoadCredentials()
			if err != nil {
				return err
			}
			if creds == nil || creds.AccessToken == "" {
				fmt.Println("not signed in — run `digitorn login`")
				return nil
			}
			label := lipgloss.NewStyle().Bold(true)
			muted := lipgloss.NewStyle().Foreground(lipgloss.Color("#9a9aa3"))
			if creds.Email != "" {
				fmt.Printf("%s %s\n", label.Render("email   "), creds.Email)
			}
			if creds.Name != "" {
				fmt.Printf("%s %s\n", label.Render("name    "), creds.Name)
			}
			if creds.UserID != "" {
				fmt.Printf("%s %s\n", label.Render("user_id "), muted.Render(creds.UserID))
			}
			if creds.AuthURL != "" {
				fmt.Printf("%s %s\n", label.Render("issuer  "), muted.Render(creds.AuthURL))
			}
			if creds.ExpiresAt > 0 {
				exp := time.Unix(int64(creds.ExpiresAt), 0).Local()
				remaining := time.Until(exp).Round(time.Second)
				status := muted.Render(exp.Format(time.RFC3339))
				if remaining < 0 {
					status = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff4e4e")).Render("EXPIRED — run `digitorn login`")
				} else {
					status = fmt.Sprintf("%s (in %s)", status, remaining)
				}
				fmt.Printf("%s %s\n", label.Render("expires "), status)
			}
			return nil
		},
	}
}

func runLogin(ctx context.Context, issuer, provider string, force bool) error {
	if issuer == "" {
		issuer = os.Getenv("DIGITORN_AUTH_URL")
	}
	if !force {
		existing, _ := client.LoadCredentials()
		if existing != nil && !existing.IsExpired(0) {
			who := existing.Email
			if who == "" {
				who = existing.UserID
			}
			fmt.Printf("already signed in as %s\n", who)
			fmt.Println("run with --force to re-login or `digitorn logout` first")
			return nil
		}
	}

	fmt.Printf("opening your browser to sign in via %s...\n", provider)
	cfg := client.OAuthConfig{
		Issuer:   issuer,
		Provider: provider,
		Timeout:  3 * time.Minute,
		PromptUser: func(authorizeURL string) {
			fmt.Println()
			fmt.Println("If your browser doesn't open automatically, paste this URL :")
			fmt.Println()
			fmt.Println("  " + authorizeURL)
			fmt.Println()
		},
	}
	creds, err := client.Login(ctx, cfg)
	if err != nil {
		return fmt.Errorf("login failed : %w", err)
	}
	if err := client.SaveCredentials(creds); err != nil {
		return fmt.Errorf("save credentials : %w", err)
	}
	who := creds.Email
	if who == "" {
		who = creds.UserID
	}
	fmt.Printf("\n✓ signed in as %s\n", who)
	if path, _ := client.CredentialsPath(); path != "" {
		fmt.Printf("  credentials saved to %s\n", path)
	}
	return nil
}
