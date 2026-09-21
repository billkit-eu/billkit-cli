package cli

import (
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/billkit-eu/billkit-cli/internal/config"
	"github.com/spf13/cobra"
)

func logoutCmd(g *globals) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove stored credentials for a profile",
		Long: "Delete a profile's API key from ~/.billkit/config.json. Targets the\n" +
			"--profile you name (else the default profile); pass --all to remove every\n" +
			"profile at once.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogout(cmd.OutOrStdout(), cmd.ErrOrStderr(), g, all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "remove every configured profile")
	return cmd
}

// runLogout removes the targeted profile(s). Reads the shared --profile flag,
// else BILLKIT_PROFILE, else the default profile, unless `all` is set. It
// honours the same selector every other command does on purpose: a profile
// that the environment has been steering every call at is the profile the user
// means when they say "log out".
func runLogout(out, errOut io.Writer, g *globals, all bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(cfg.Profiles) == 0 {
		fmt.Fprintln(out, "No profiles configured — nothing to log out of.")
		return nil
	}

	if all {
		names := sortedProfileNames(cfg)
		cfg.Profiles = map[string]config.Profile{}
		cfg.DefaultProfile = ""
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(out, "✓ Logged out of all profiles (%v).\n", names)
		return nil
	}

	name := profileOverride(g)
	if name == "" {
		name = cfg.DefaultProfile
	}
	if name == "" {
		return fmt.Errorf("no default profile — pass --profile <name> or --all")
	}
	wasDefault := cfg.DefaultProfile == name
	if !cfg.Delete(name) {
		return fmt.Errorf("no such profile %q", name)
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ Logged out of the %q profile.\n", name)
	if wasDefault {
		reportProfileAfterLogout(errOut, cfg)
	}
	return nil
}

// reportProfileAfterLogout says what the next command will actually use.
//
// Delete no longer promotes a survivor, so logging out of the default leaves
// no default at all. Staying silent about that is how logging out of "test"
// used to point the next command at live money. Resolve still falls back to a
// sole remaining profile, so when one survives we name it and its mode rather
// than implying nothing is selected.
func reportProfileAfterLogout(errOut io.Writer, cfg *config.Config) {
	names := sortedProfileNames(cfg)
	switch len(names) {
	case 0:
		return
	case 1:
		only := names[0]
		mode := config.Mode(cfg.Profiles[only].APIKey)
		if mode == "live" {
			fmt.Fprintf(errOut,
				"! %q is the only profile left, so commands will now use it. It is a LIVE key.\n"+
					"  Log out of it too with: billkit logout --profile %s\n", only, only)
			return
		}
		fmt.Fprintf(errOut, "> %q is the only profile left, so commands will now use it (%s mode).\n", only, mode)
	default:
		fmt.Fprintf(errOut,
			"> No default profile is set. Choose one with: billkit config use <profile>\n"+
				"  Configured: %v\n", names)
	}
}

func sortedProfileNames(cfg *config.Config) []string {
	return slices.Sorted(maps.Keys(cfg.Profiles))
}
