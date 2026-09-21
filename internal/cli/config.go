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
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if len(cfg.Profiles) == 0 {
				fmt.Println("No profiles configured. Run `billkit login`.")
				return nil
			}
			for name, p := range cfg.Profiles {
				marker := " "
				if name == cfg.DefaultProfile {
					marker = "*"
				}
				base := p.BaseURL
				if base == "" {
					base = config.DefaultBaseURL
				}
				fmt.Printf("%s %-6s  %s…  %s\n", marker, name, maskKey(p.APIKey), base)
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
		RunE: func(_ *cobra.Command, args []string) error {
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
			fmt.Printf("✓ Profile %q is now the default.\n", name)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the config file location",
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			fmt.Println(path)
			return nil
		},
	})

	return cmd
}

func maskKey(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12]
}
