package cli

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// buildDetails is what `billkit version` reports.
type buildDetails struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// buildInfo resolves the commit and date: the ldflags a release sets, or
// failing that the VCS stamp `go build` records from a git checkout, or
// "unknown". A `go install …@v1.0.0` build has neither, because the module
// proxy serves a zip with no .git, so it honestly says unknown rather than
// inventing one.
func buildInfo() buildDetails {
	d := buildDetails{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		stamped, modified := d.Commit == "", false
		for _, s := range info.Settings {
			switch {
			case s.Key == "vcs.revision" && d.Commit == "":
				d.Commit = s.Value
			case s.Key == "vcs.time" && d.Date == "":
				d.Date = s.Value
			case s.Key == "vcs.modified":
				modified = s.Value == "true"
			}
		}
		// A local build from a tree with uncommitted changes is not that
		// commit, and saying so saves someone a confusing bug report.
		if stamped && modified && d.Commit != "" {
			d.Commit += "-dirty"
		}
	}
	if d.Commit == "" {
		d.Commit = "unknown"
	}
	if d.Date == "" {
		d.Date = "unknown"
	}
	return d
}

// versionLine is the one line `billkit version` and `billkit --version`
// print. One line, with the version as the third word, so `awk '{print $3}'`
// gets the version and nothing has to parse a block.
func versionLine() string {
	d := buildInfo()
	commit, dirty := strings.CutSuffix(d.Commit, "-dirty")
	if len(commit) > 12 {
		commit = commit[:12]
	}
	if dirty {
		commit += "-dirty"
	}
	return fmt.Sprintf("billkit version %s (commit %s, built %s, %s %s/%s)", d.Version, commit, d.Date, d.Go, d.OS, d.Arch)
}

func versionCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version, commit and build date",
		Long: "Print the version, the commit it was built from and when, one line.\n" +
			"With --json, print the same as an object (full commit hash).\n\n" +
			"billkit makes no network call to answer this, and never checks for\n" +
			"updates on its own.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.json {
				raw, err := json.Marshal(buildInfo())
				if err != nil {
					return err
				}
				fprintJSON(cmd.OutOrStdout(), g.color, raw)
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), versionLine())
			return nil
		},
	}
}
