package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/billkit-eu/billkit-cli/internal/api"
	"github.com/billkit-eu/billkit-cli/internal/config"
	"github.com/spf13/cobra"
)

// loginProfileName decides which profile this key is stored under: --profile,
// else BILLKIT_PROFILE, else the mode the key's prefix names.
//
// Every other command already resolves an arbitrary profile name, and `config
// use` and `logout` already operate on one, so login was the only place that
// pretended the set was fixed at {test, live}: `--profile staging` was
// accepted on the command line, silently ignored, and the key landed under
// "test" — which the next `--profile staging` then could not find.
//
// The one name it refuses is the other mode's. "test" and "live" are the names
// the CLI assigns from the key prefix, and they are what `config list`,
// `logout` and the live-mode banner report back; a live key stored under
// "test" makes every one of those read as a reassurance that is false. Any
// other name is the user's to choose.
func loginProfileName(g *globals, mode string) (string, error) {
	name := strings.TrimSpace(profileOverride(g))
	if name == "" {
		return mode, nil
	}
	if other := map[string]string{"test": "live", "live": "test"}[name]; other == mode {
		return "", fmt.Errorf(
			"refusing to store a %s key under the profile name %q: that name is reserved for %s keys, "+
				"and `billkit config list` would report this one as %s. Pick another name, or omit --profile to use %q",
			mode, name, name, name, mode)
	}
	return name, nil
}

func loginCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Store an API key for a profile",
		Long: "Store a BillKit API key (bk_test_… / bk_live_…) in ~/.billkit/config.json.\n" +
			"The key's prefix names the profile (test/live) unless --profile says\n" +
			"otherwise, and the key is validated against the API before being saved.\n\n" +
			"  billkit login --profile staging   # store under a name of your choosing\n\n" +
			"The prompt does not echo what you type. To skip it non-interactively,\n" +
			"set BILLKIT_API_KEY or pipe the key in (`cat key.txt | billkit login`).\n" +
			"--api-key works too but puts the secret in the process list and in your\n" +
			"shell history, so prefer either of the other two.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			key, source := apiKeyOverride(g)
			switch {
			case key == "":
				var err error
				key, err = readSecretLine(cmd.InOrStdin(), cmd.ErrOrStderr(),
					"Enter your BillKit API key (bk_test_… / bk_live_…): ")
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
				return fmt.Errorf("that doesn't look like a BillKit key (expected bk_test_… or bk_live_…)")
			}
			// Before the key reaches the wire. A name this command is going to
			// refuse should not cost a round trip first, and "your key is
			// fine, but" is a confusing way to report a flag error.
			name, err := loginProfileName(g, mode)
			if err != nil {
				return err
			}

			baseURL := baseURLOverride(g)
			if baseURL == "" {
				baseURL = config.DefaultBaseURL
			}
			// Refuse before the key ever reaches the wire, not after.
			if err := config.CheckTransport(baseURL, key); err != nil {
				return err
			}

			// Validate the key with a cheap authenticated call.
			//
			// GET /v1/ping, and that choice is load-bearing. This used to
			// call /v1/tenant/capabilities, which needs `tenant:read`, so a
			// key scoped to refunds or checkout could never log in and was
			// never saved -- the CLI refused a credential that was entirely
			// valid for the work it was minted to do. /v1/ping is the one
			// route on the scoped /v1 router that is exempt from the scope
			// check while still requiring a principal, so every real key
			// reaches it and no key's scope set can turn login into a
			// failure.
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			c := api.New(baseURL, key, Version, newHTTPClient(15*time.Second))
			if _, err := c.Do(ctx, "GET", "/v1/ping", nil); err != nil {
				// A 403 still proves the key authenticated: the server
				// checks the credential before it checks authorization, so
				// there is no way to be refused for scope without having
				// been identified first. Belt to /v1/ping's braces -- it
				// keeps login working against a deployment that has somehow
				// put a scope on ping, instead of failing for a reason that
				// has nothing to do with whether the key is real.
				var apiErr *api.APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
					return fmt.Errorf("could not validate the key against %s: %w", baseURL, err)
				}
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			p := config.Profile{APIKey: key}
			if baseURL != config.DefaultBaseURL {
				p.BaseURL = baseURL
			}
			cfg.Set(name, p)
			// The profile you just authenticated is the one you meant to use.
			// Without this, logging in with a test key while a live key was
			// already stored leaves every later command on the live key --
			// the CLI says "test mode" and then spends real money.
			cfg.SetDefault(name)
			if err := cfg.Save(); err != nil {
				return err
			}

			path, _ := config.Path()
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Logged in (%s mode). Profile %q is now the default. Credentials saved to %s\n", mode, name, path)
			return nil
		},
	}
}
