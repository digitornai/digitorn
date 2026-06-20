package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// NewSecret returns the `digitorn secret` command group for managing
// per-app encrypted secrets.
func NewSecret() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage per-app secrets",
		Long: `Manage encrypted secrets for a digitorn app.

Secrets are stored encrypted at rest and injected into the agent's
environment at runtime.`,
	}
	cmd.AddCommand(
		newSecretList(),
		newSecretGet(),
		newSecretSet(),
		newSecretDelete(),
	)
	return cmd
}

func newSecretList() *cobra.Command {
	return &cobra.Command{
		Use:   "list <app-id>",
		Short: "List all secret keys for an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			appID := args[0]
			c, err := dialClient(cmd.Context())
			if err != nil {
				return err
			}
			secrets, err := c.ListSecrets(cmd.Context(), appID)
			if err != nil {
				return fmt.Errorf("list secrets: %w", err)
			}
			if len(secrets) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No secrets for app %q.\n", appID)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Secrets for %s:\n", appID)
			for k := range secrets {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", k)
			}
			return nil
		},
	}
}

func newSecretGet() *cobra.Command {
	return &cobra.Command{
		Use:   "get <app-id> <key>",
		Short: "Get a secret value",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			appID, key := args[0], args[1]
			c, err := dialClient(cmd.Context())
			if err != nil {
				return err
			}
			val, err := c.GetSecret(cmd.Context(), appID, key)
			if err != nil {
				return fmt.Errorf("get secret: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), val)
			return nil
		},
	}
}

func newSecretSet() *cobra.Command {
	return &cobra.Command{
		Use:   "set <app-id> <key> [value]",
		Short: "Set a secret value",
		Long: `Set a secret for an app. If value is omitted, reads from stdin.
The value is stored encrypted at rest on the daemon.`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			appID, key := args[0], args[1]
			var value string
			if len(args) >= 3 {
				value = args[2]
			} else {
				data, err := readStdin()
				if err != nil {
					return err
				}
				value = strings.TrimSpace(string(data))
			}
			c, err := dialClient(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.SetSecret(cmd.Context(), appID, key, value); err != nil {
				return fmt.Errorf("set secret: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Secret %s set for app %s.\n", key, appID)
			return nil
		},
	}
}

func newSecretDelete() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <app-id> <key>",
		Short: "Delete a secret",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			appID, key := args[0], args[1]
			c, err := dialClient(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.DeleteSecret(cmd.Context(), appID, key); err != nil {
				return fmt.Errorf("delete secret: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Secret %s deleted from app %s.\n", key, appID)
			return nil
		},
	}
}

// readStdin reads all available data from stdin, used when
// a secret value is piped in.
func readStdin() ([]byte, error) {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return nil, err
	}
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		return nil, fmt.Errorf("value required (provide as argument or pipe to stdin)")
	}
	data := make([]byte, 4096)
	n, err := os.Stdin.Read(data)
	if err != nil {
		return nil, err
	}
	return data[:n], nil
}
