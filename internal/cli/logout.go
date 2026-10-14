package cli

import (
	"fmt"
	"io"

	"github.com/billkit-eu/billkit-cli/internal/config"
	"github.com/spf13/cobra"
)

func logoutCmd(g *globals) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove stored credentials for a profile",
		Long: "Delete a profile's API key from the config file (`billkit config path`),\n" +
			"and from the OS keychain when that is where it was kept. Targets the\n" +
			"--profile you name (else the default profile); pass --all to remove every\n" +
			"profile at once.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogout(cmd.ErrOrStderr(), g, all)
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
//
// Everything it prints is a message, not data, so all of it goes to errOut
// (stderr) and stdout stays empty.
func runLogout(errOut io.Writer, g *globals, all bool) error {
	// The whole read-decide-write runs under the config lock (config.Update),
	// so a login racing this logout can neither be undone by it nor undo it.
	var done func()
	// Confirmations are notices (silent under --json); the one warning in
	// reportProfileAfterLogout, a sole live survivor, is not.
	note := g.notices(errOut)
	err := config.Update(func(cfg *config.Config) error {
		// An explicit name that is not configured is the same bad invocation
		// whether or not anything else is: `config use` and `--profile` on
		// every other command answer it the same way.
		name := profileOverride(g)
		if !all && name != "" {
			if _, ok := cfg.Profiles[name]; !ok {
				return &UsageError{cfg.UnknownProfileError(name)}
			}
		}
		if len(cfg.Profiles) == 0 {
			done = func() { fmt.Fprintln(note, "No profiles configured, so there is nothing to log out of.") }
			return config.ErrSkipSave
		}

		if all {
			names := sortedProfileNames(cfg)
			// Through Delete, one at a time, so a key kept in the keychain
			// is removed from it too rather than orphaned there.
			for _, n := range names {
				cfg.Delete(n)
			}
			done = func() { fmt.Fprintf(note, "✓ Logged out of all profiles (%v).\n", names) }
			return nil
		}

		if name == "" {
			name = cfg.DefaultProfile
		}
		if name == "" {
			return usageErrorf("no default profile; pass --profile <name> or --all")
		}
		wasDefault := cfg.DefaultProfile == name
		if !cfg.Delete(name) {
			return &UsageError{cfg.UnknownProfileError(name)}
		}
		done = func() {
			fmt.Fprintf(note, "✓ Logged out of the %q profile.\n", name)
			if wasDefault {
				reportProfileAfterLogout(errOut, note, cfg)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	done()
	return nil
}

// reportProfileAfterLogout says what the next command will actually use.
//
// Delete no longer promotes a survivor, so logging out of the default leaves
// no default at all. Staying silent about that is how logging out of "test"
// used to point the next command at live money. Resolve still falls back to a
// sole remaining profile, so when one survives we name it and its mode rather
// than implying nothing is selected.
func reportProfileAfterLogout(errOut, note io.Writer, cfg *config.Config) {
	names := sortedProfileNames(cfg)
	switch len(names) {
	case 0:
		return
	case 1:
		only := names[0]
		mode := cfg.Profiles[only].Mode()
		if mode == "live" {
			fmt.Fprintf(errOut,
				"! %q is the only profile left, so commands will now use it. It is a LIVE key.\n"+
					"  Log out of it too with: billkit logout --profile %s\n", only, only)
			return
		}
		fmt.Fprintf(note, "> %q is the only profile left, so commands will now use it (%s mode).\n", only, mode)
	default:
		fmt.Fprintf(note,
			"> No default profile is set. Choose one with: billkit config use <profile>\n"+
				"  Configured: %v\n", names)
	}
}

func sortedProfileNames(cfg *config.Config) []string {
	return cfg.ProfileNames()
}
