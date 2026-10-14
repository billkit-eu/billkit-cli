package cli

import (
	"encoding/json"
	"fmt"

	"github.com/billkit-eu/billkit-cli/internal/config"
	"github.com/spf13/cobra"
)

// configCmd reads and writes the config file directly and never resolves a
// credential, so of the globals it reads only --json and --color.
func configCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the CLI configuration",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List configured profiles",
		Long: "List the stored profiles, one per line, with * on the default. Keys are\n" +
			"masked, and a key kept in the OS keychain is shown as such without being\n" +
			"read. With --json, print an object instead (see the CLI reference for its\n" +
			"fields).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if g.json {
				return printConfigJSON(cmd, g, cfg)
			}
			out := cmd.OutOrStdout()
			if len(cfg.Profiles) == 0 {
				// A notice, not data: stdout stays empty, so a script
				// counting lines counts zero profiles.
				fmt.Fprintln(cmd.ErrOrStderr(), "No profiles configured. Run `billkit login`.")
				return nil
			}
			// Sorted, because ranging a map prints the profiles in a
			// different order on every run, and this is the command people
			// read to check which one is the default.
			for _, name := range cfg.ProfileNames() {
				p := cfg.Profiles[name]
				marker := " "
				if name == cfg.DefaultProfile {
					marker = "*"
				}
				base := p.BaseURL
				if base == "" {
					base = config.DefaultBaseURL
				}
				fmt.Fprintf(out, "%s %-8s  %-22s  %s\n", marker, name, displayKey(p), base)
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "use <profile>",
		Short: "Make a profile the default for later commands",
		Long: "Switch which stored profile later commands use, without deleting any\n" +
			"credentials.\n\n" +
			"  billkit config use test",
		Args: cobra.ExactArgs(1),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			// Read-only: a Tab press must never be what moves a pre-1.0
			// config or writes to the keychain.
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return cfg.ProfileNames(), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := config.Update(func(cfg *config.Config) error {
				if !cfg.SetDefault(name) {
					// The same answer `logout --profile` and `--profile` on
					// any command give a name that is not configured: a bad
					// invocation, exit 2, naming the ones that are.
					return &UsageError{cfg.UnknownProfileError(name)}
				}
				return nil
			}); err != nil {
				return err
			}
			fmt.Fprintf(g.notices(cmd.ErrOrStderr()), "✓ Profile %q is now the default.\n", name)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the config file location",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	})

	return cmd
}

// configProfileJSON is one profile in `config list --json`.
type configProfileJSON struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
	Mode    string `json:"mode"`
	APIKey  string `json:"api_key"`
	Storage string `json:"key_storage"`
	BaseURL string `json:"base_url"`
}

// printConfigJSON is `config list --json`. The key is masked exactly as in
// the table: the JSON form exists for scripts, and a script has even less
// business holding a full secret than a terminal does.
func printConfigJSON(cmd *cobra.Command, g *globals, cfg *config.Config) error {
	path, err := config.Path()
	if err != nil {
		return err
	}
	out := struct {
		Path           string              `json:"config_path"`
		DefaultProfile string              `json:"default_profile"`
		Profiles       []configProfileJSON `json:"profiles"`
	}{Path: path, DefaultProfile: cfg.DefaultProfile, Profiles: []configProfileJSON{}}
	for _, name := range cfg.ProfileNames() {
		p := cfg.Profiles[name]
		base := p.BaseURL
		if base == "" {
			base = config.DefaultBaseURL
		}
		out.Profiles = append(out.Profiles, configProfileJSON{
			Name: name, Default: name == cfg.DefaultProfile, Mode: p.Mode(),
			APIKey: displayKey(p), Storage: p.Storage(), BaseURL: base,
		})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	fprintJSON(cmd.OutOrStdout(), g.color, raw)
	return nil
}

// displayKey is how a stored profile's key is shown. A keychain key is not
// read just to be masked: reading it can prompt, and the prefix is already
// known, because only live keys are ever put there.
func displayKey(p config.Profile) string {
	if p.Keychain != "" {
		return "bk_live_… (keychain)"
	}
	return maskKey(p.APIKey)
}

// maskKey renders a stored key for display: the bk_live_/bk_test_ prefix plus
// four characters, which is enough to tell two keys apart and not enough to be
// one. It returns the complete display string, ellipsis included, because the
// caller used to append that itself, and the old length guard returned the
// *whole* key for anything 12 characters or shorter, so a truncated or
// hand-edited config.json printed a key in full while looking exactly like a
// masked one. A masking function that silently does not mask is worse than no
// masking at all, so the short case now shows nothing of the value.
func maskKey(key string) string {
	const shown = 12 // len("bk_live_") + 4
	if len(key) <= shown {
		if key == "" {
			return "(no key)"
		}
		return "(malformed key)"
	}
	return key[:shown] + "…"
}
