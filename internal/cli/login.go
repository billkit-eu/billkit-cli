package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/billkit-eu/billkit-cli/internal/api"
	"github.com/billkit-eu/billkit-cli/internal/config"
	"github.com/spf13/cobra"
)

func loginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Store an API key for a profile",
		Long: "Store a BillKit API key (sk_test_… / sk_live_…) in ~/.billkit/config.json.\n" +
			"The key's prefix selects the profile (test/live) and the key is validated\n" +
			"against the API before being saved.\n\n" +
			"The prompt does not echo what you type. To skip it non-interactively,\n" +
			"set BILLKIT_API_KEY or pipe the key in (`cat key.txt | billkit login`).\n" +
			"--api-key works too but puts the secret in the process list and in your\n" +
			"shell history, so prefer either of the other two.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			key, source := apiKeyOverride()
			switch {
			case key == "":
				var err error
				key, err = readSecretLine(cmd.InOrStdin(), cmd.ErrOrStderr(),
					"Enter your BillKit API key (sk_test_… / sk_live_…): ")
				if err != nil {
					return err
				}
			case source == envAPIKey:
				// Say which identity is being stored. Someone who exported the
				// variable in a previous shell and then ran `billkit login`
				// expecting a prompt has to be told why there wasn't one.
				fmt.Fprintf(cmd.ErrOrStderr(), "> Using the API key from $%s.\n", envAPIKey)
			}
			mode := config.Mode(key)
			if mode == "" {
				return fmt.Errorf("that doesn't look like a BillKit key (expected sk_test_… or sk_live_…)")
			}

			baseURL := baseURLOverride()
			if baseURL == "" {
				baseURL = config.DefaultBaseURL
			}
			// Refuse before the key ever reaches the wire, not after.
			if err := config.CheckTransport(baseURL, key); err != nil {
				return err
			}

			// Validate the key with a cheap authenticated call.
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			c := api.New(baseURL, key, Version, newHTTPClient(15*time.Second))
			if _, err := c.Do(ctx, "GET", "/v1/tenant/capabilities", nil); err != nil {
				return fmt.Errorf("could not validate the key against %s: %w", baseURL, err)
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			p := config.Profile{APIKey: key}
			if baseURL != config.DefaultBaseURL {
				p.BaseURL = baseURL
			}
			cfg.Set(mode, p)
			// The profile you just authenticated is the one you meant to use.
			// Without this, logging in with a test key while a live key was
			// already stored leaves every later command on the live key --
			// the CLI says "test mode" and then spends real money.
			cfg.SetDefault(mode)
			if err := cfg.Save(); err != nil {
				return err
			}

			path, _ := config.Path()
			fmt.Printf("✓ Logged in (%s mode). Profile %q is now the default. Credentials saved to %s\n", mode, mode, path)
			return nil
		},
	}
}
