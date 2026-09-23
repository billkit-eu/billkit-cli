package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/billkit-eu/billkit-cli/internal/config"
	"github.com/spf13/cobra"
)

// The globals are accepted but unused: `config` reads and writes the config
// file directly and never resolves a credential, so it has no --profile /
// --api-key tier to consult. Taking them anyway keeps every command
// constructor the same shape.
func configCmd(_ *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the CLI configuration",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List configured profiles",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(cfg.Profiles) == 0 {
				fmt.Fprintln(out, "No profiles configured. Run `billkit login`.")
				return nil
			}
			// Sorted, because ranging a map prints the profiles in a
			// different order on every run, and this is the command people
			// read to check which one is the default.
			for _, name := range slices.Sorted(maps.Keys(cfg.Profiles)) {
				p := cfg.Profiles[name]
				marker := " "
				if name == cfg.DefaultProfile {
					marker = "*"
				}
				base := p.BaseURL
				if base == "" {
					base = config.DefaultBaseURL
				}
				fmt.Fprintf(out, "%s %-8s  %-16s  %s\n", marker, name, maskKey(p.APIKey), base)
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
			cfg, err := config.Load()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return slices.Sorted(maps.Keys(cfg.Profiles)), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			name := args[0]
			if !cfg.SetDefault(name) {
				known := slices.Sorted(maps.Keys(cfg.Profiles))
				if len(known) == 0 {
					return fmt.Errorf("no profiles configured — run `billkit login`")
				}
				return fmt.Errorf("no such profile %q — configured profiles: %s", name, strings.Join(known, ", "))
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Profile %q is now the default.\n", name)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the config file location",
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

// maskKey renders a stored key for display: the bk_live_/bk_test_ prefix plus
// four characters, which is enough to tell two keys apart and not enough to be
// one. It returns the complete display string, ellipsis included, because the
// caller used to append that itself — and the old length guard returned the
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
