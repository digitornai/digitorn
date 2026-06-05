package commands

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbathepaul/digitorn-cli/internal/client"
)

// loadFreshCreds loads the local credentials and, if the access token is
// expired (or within 60s of it), transparently refreshes it with the
// stored refresh_token and saves the result. Returns (nil, nil) when no
// credentials exist (dev-mode daemon accepts unauthenticated). Only errors
// when refresh is needed but impossible.
func loadFreshCreds(ctx context.Context) (*client.Credentials, error) {
	creds, err := client.LoadCredentials()
	if err != nil {
		return nil, err
	}
	if creds == nil {
		return nil, nil
	}
	if !creds.IsExpired(60 * time.Second) {
		return creds, nil
	}
	if creds.RefreshToken == "" {
		return nil, fmt.Errorf("session expired — run `digitorn login` to renew")
	}
	fresh, err := client.RefreshAccessToken(ctx, client.OAuthConfig{Issuer: creds.AuthURL}, creds.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("session expired and auto-refresh failed (%v) — run `digitorn login`", err)
	}
	// Preserve metadata the refresh response may have omitted.
	if fresh.UserID == "" {
		fresh.UserID = creds.UserID
	}
	if fresh.Email == "" {
		fresh.Email = creds.Email
	}
	if fresh.Name == "" {
		fresh.Name = creds.Name
	}
	if fresh.AuthURL == "" {
		fresh.AuthURL = creds.AuthURL
	}
	_ = client.SaveCredentials(fresh)
	return fresh, nil
}

// dialClient resolves the daemon URL, health-checks it, and builds a
// client bound to fresh local credentials (auto-refreshing if expired).
// Shared by the app-management commands so the boilerplate lives once.
func dialClient(ctx context.Context) (*client.Client, error) {
	creds, err := loadFreshCreds(ctx)
	if err != nil {
		return nil, err
	}
	url, err := client.DiscoverAndPing(ctx, 0)
	if err != nil {
		if client.IsUnreachable(err) {
			return nil, fmt.Errorf("%w\n\nIs the daemon running ?", err)
		}
		return nil, err
	}
	return client.New(clientOptions(url, creds))
}

// clientOptions builds the client.Options from creds, wiring transparent
// 401 token-refresh : the refresh token + auth URL let the client renew
// expired access tokens mid-flight, and the callback persists the fresh
// credentials so the next process already has them.
func clientOptions(url string, creds *client.Credentials) client.Options {
	opts := client.Options{
		BaseURL:     url,
		BearerToken: tokenFromCreds(creds),
		UserID:      client.DefaultUserID(creds),
	}
	if creds != nil {
		opts.RefreshToken = creds.RefreshToken
		opts.AuthURL = creds.AuthURL
		opts.OnTokenRefresh = func(fresh *client.Credentials) {
			mergeCredMeta(fresh, creds)
			_ = client.SaveCredentials(fresh)
		}
	}
	return opts
}

// mergeCredMeta fills metadata the refresh response may have omitted from
// the previous credentials, so persisted creds never lose the email /
// user_id / issuer.
func mergeCredMeta(fresh, prev *client.Credentials) {
	if fresh == nil || prev == nil {
		return
	}
	if fresh.UserID == "" {
		fresh.UserID = prev.UserID
	}
	if fresh.Email == "" {
		fresh.Email = prev.Email
	}
	if fresh.Name == "" {
		fresh.Name = prev.Name
	}
	if fresh.AuthURL == "" {
		fresh.AuthURL = prev.AuthURL
	}
}

// NewInstall returns `digitorn install <source>`.
func NewInstall() *cobra.Command {
	return &cobra.Command{
		Use:   "install <source>",
		Short: "Install (or upgrade) an app",
		Long: "Install an app bundle. <source> can be :\n" +
			"  /abs/or/relative/path     a local directory or archive\n" +
			"  hub://publisher/pkg@1.0   the digitorn hub\n" +
			"  builtin://name            a built-in example",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInstall(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
}

func runInstall(ctx context.Context, out io.Writer, source string) error {
	// Resolve a local path to absolute so the daemon (different cwd)
	// reads the right bundle ; leave URIs (scheme://) untouched.
	src := source
	if !strings.Contains(src, "://") {
		if abs, err := filepath.Abs(src); err == nil {
			src = abs
		}
	}
	c, err := dialClient(ctx)
	if err != nil {
		return err
	}
	resp, err := c.InstallApp(ctx, src)
	if err != nil {
		return fmt.Errorf("install : %w", err)
	}
	fmt.Fprintf(out, "installed %s (%s) v%s\n", resp.AppID, resp.Name, resp.Version)
	return nil
}

// NewUninstall returns `digitorn uninstall <app-id> [--purge]`.
func NewUninstall() *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "uninstall <app-id>",
		Short: "Remove an installed app",
		Long:  "Uninstall an app. Pass --purge to also delete its stored data (sessions, workspace).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dialClient(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.UninstallApp(cmd.Context(), args[0], purge); err != nil {
				return fmt.Errorf("uninstall : %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "uninstalled %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the app's stored data")
	return cmd
}

// NewEnable returns `digitorn enable <app-id>`.
func NewEnable() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <app-id>",
		Short: "Enable a disabled app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dialClient(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.EnableApp(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("enable : %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "enabled %s\n", args[0])
			return nil
		},
	}
}

// NewDisable returns `digitorn disable <app-id>`.
func NewDisable() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <app-id>",
		Short: "Disable an app without uninstalling it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dialClient(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.DisableApp(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("disable : %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "disabled %s\n", args[0])
			return nil
		},
	}
}
