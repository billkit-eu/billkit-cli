// Package cli wires the billkit CLI's command tree.
//
// The entry point that calls Execute is the main package at cmd/billkit, whose
// directory name is what `go install` names the binary. That is the only reason
// this package does not sit at the module root: Go names an installed binary
// after the last element of the main package's import path, so `package main`
// has to live in a directory called `billkit`.
package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/billkit-eu/billkit-cli/internal/api"
	"github.com/billkit-eu/billkit-cli/internal/config"
	"github.com/spf13/cobra"
)

// Version is the version this source declares, and the single place it is
// written down. GoReleaser overrides it at build time from the release tag
// (-ldflags "-X …/internal/cli.Version=X.Y.Z"), so a released binary reports
// the tag and a `go install` or `go build` of this tree reports the version the
// tree is at, rather than the "dev" it used to claim.
//
// The release scripts read this line: `sdk/scripts/lib.sh` parses it to decide
// which version the CLI declares, `sdk/scripts/release.sh` refuses to tag a
// version that disagrees with it, and the mirror's publish workflow re-checks
// the pushed tag against it before GoReleaser runs. So bump it here, split, then
// tag. See sdk/RELEASING.md.
var Version = "0.2.3"

// globals holds the values behind the root command's persistent flags.
//
// One instance is created by rootCmd() and handed to every subcommand
// constructor. Package-level flag variables would be simpler to write and
// wrong in the two ways that matter: two command trees in one process
// (tests, an embedding caller) would share them, and a test that set one
// would leak it into the next test unless it remembered to restore it —
// which is exactly the bug class a CLI that reads credentials should not
// be exposed to.
type globals struct {
	profile string
	apiKey  string
	baseURL string
	color   string
	yes     bool

	// announcedHost keeps the non-default-host notice to one line per
	// command tree. It was a package-level sync.Once, which meant a second
	// tree in the same process stayed silent about a host the first one had
	// already named — and that notice exists precisely so a repointed CLI
	// cannot carry a key somewhere quietly.
	announcedHost bool
}

// Deadlines. Reads are cheap to repeat, so they stay short. Money-moving
// writes get far longer: the API makes a live Mollie round trip inside the
// request, and a client that gives up while the provider is still working is
// what manufactures the "did it happen?" ambiguity in the first place.
const (
	readTimeout  = 30 * time.Second
	writeTimeout = 80 * time.Second
)

func rootCmd() *cobra.Command {
	g := &globals{color: "auto"}
	root := &cobra.Command{
		Use:   "billkit",
		Short: "The BillKit command-line interface",
		Long: "billkit is the command-line interface for the BillKit billing API.\n\n" +
			"Forward live events to your local app with `billkit listen`, fire test\n" +
			"events with `billkit trigger`, create refunds and one-shot payments, and\n" +
			"hit any route with `billkit api`.\n\n" +
			"  billkit login\n" +
			"  billkit listen --forward-to http://localhost:3000/webhook\n" +
			"  billkit refunds create --payment pay_123 --amount 500\n" +
			"  billkit checkout one-shot --customer cus_1 --amount 1999 --method ideal \\\n" +
			"      --success-url https://example.com/thanks",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}

	root.PersistentFlags().StringVar(&g.profile, "profile", "", "config profile to use (env: BILLKIT_PROFILE; default: the configured default)")
	root.PersistentFlags().StringVar(&g.apiKey, "api-key", "", "API key for this call; prefer BILLKIT_API_KEY, since a flag value is visible in the process list and in shell history")
	root.PersistentFlags().StringVar(&g.baseURL, "base-url", "", "API host override (env: BILLKIT_BASE_URL; default: https://api.billkit.eu)")
	root.PersistentFlags().StringVar(&g.color, "color", "auto", "colorize JSON output: auto, always, or never")
	// Named to match the internal `bill` CLI's global --yes/-y.
	root.PersistentFlags().BoolVarP(&g.yes, "yes", "y", false, "confirm live-mode money commands without prompting (required in scripts)")

	root.AddCommand(
		loginCmd(g),
		logoutCmd(g),
		configCmd(g),
		listenCmd(g),
		triggerCmd(g),
		eventsCmd(g),
		refundsCmd(g),
		checkoutCmd(g),
		apiCmd(g),
	)
	return root
}

// Execute runs the CLI.
func Execute() error {
	return rootCmd().Execute()
}

// creds is the identity one invocation runs as.
type creds struct {
	apiKey    string
	baseURL   string
	profile   string // stored profile name; empty when a key was supplied directly
	keySource string // "--api-key" or "BILLKIT_API_KEY"; empty when it came from a profile
	mode      string // "live", "test", or "" when the key prefix is unrecognised
}

// origin describes, for a human, where this invocation's key came from, or ""
// when there is nothing useful to say. It names the source, never the key.
func (c creds) origin() string {
	switch {
	case c.profile != "":
		return fmt.Sprintf("profile %q", c.profile)
	case c.keySource != "":
		return c.keySource
	default:
		return ""
	}
}

// isDefaultHost reports whether this invocation talks to the official API.
func (c creds) isDefaultHost() bool { return c.baseURL == config.DefaultBaseURL }

// resolve returns the API key + base URL for this invocation: flag first, then
// the environment, then the stored profile. See env.go for why that order.
//
// An explicitly supplied key (from either tier) means the config file is not
// consulted at all, so a CI runner with BILLKIT_API_KEY set needs no config
// file and a broken one cannot get in its way. The transport check below runs
// on every tier, so a key from the environment is held to the same rule as one
// from a flag.
func resolve(g *globals) (creds, error) {
	var out creds
	key, keySource := apiKeyOverride(g)
	baseURL := baseURLOverride(g)
	if key != "" {
		out = creds{apiKey: key, baseURL: baseURL, keySource: keySource}
		if out.baseURL == "" {
			out.baseURL = config.DefaultBaseURL
		}
	} else {
		cfg, err := config.Load()
		if err != nil {
			return creds{}, err
		}
		profile, name, err := cfg.Resolve(profileOverride(g))
		if err != nil {
			// The config package knows about stored profiles and nothing
			// else, so it can only name `billkit login`. Someone hitting this
			// in a script wants the option that does not write a key to the
			// runner's disk, so say it here, where the tiers are known.
			if errors.Is(err, config.ErrNoProfile) {
				return creds{}, fmt.Errorf("%w, or set %s, or pass --api-key", err, envAPIKey)
			}
			return creds{}, err
		}
		out = creds{apiKey: profile.APIKey, baseURL: profile.BaseURL, profile: name}
		if baseURL != "" {
			out.baseURL = baseURL
		}
	}

	if out.apiKey != "" {
		if err := config.CheckTransport(out.baseURL, out.apiKey); err != nil {
			return creds{}, err
		}
	} else if err := config.ValidateBaseURL(out.baseURL); err != nil {
		return creds{}, err
	}
	out.mode = config.Mode(out.apiKey)
	return out, nil
}

// client builds an authenticated API client for a read or a routine call.
func client(cmd *cobra.Command, g *globals) (*api.Client, error) {
	c, _, err := clientWithTimeout(cmd, g, readTimeout)
	return c, err
}

// clientWithTimeout builds an authenticated client and reports the identity
// it runs as, so money commands can say out loud which mode they are in.
func clientWithTimeout(cmd *cobra.Command, g *globals, timeout time.Duration) (*api.Client, creds, error) {
	cr, err := resolve(g)
	if err != nil {
		return nil, creds{}, err
	}
	if cr.apiKey == "" {
		return nil, creds{}, fmt.Errorf("no API key: run `billkit login`, or set %s (preferred in scripts), or pass --api-key", envAPIKey)
	}
	announceHost(cmd.ErrOrStderr(), g, cr)
	// The transport cap sits just above the per-call context deadline so the
	// context is what fires, and the error names the deadline the user can
	// reason about.
	return api.New(cr.baseURL, cr.apiKey, Version, newHTTPClient(timeout+5*time.Second)), cr, nil
}

// transportOverride is a test seam. It lets the package's own tests trust an
// httptest TLS server's self-signed certificate without loosening any of the
// transport rules the CLI enforces. It is nil in every shipped build.
var transportOverride http.RoundTripper

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: transportOverride}
}

// announceHost says out loud, once per command tree, when the CLI is pointed
// somewhere other than the official API. A repointed CLI should announce
// itself rather than quietly carry the key off to a host the user did not
// expect.
//
// It writes to the command's own error stream, not os.Stderr, so the notice
// lands wherever the caller redirected the command — which is also what makes
// it assertable without swapping a package variable out from under the test.
func announceHost(w io.Writer, g *globals, cr creds) {
	if cr.isDefaultHost() || g.announcedHost {
		return
	}
	g.announcedHost = true
	fmt.Fprintf(w, "> Using API host %s\n", cr.baseURL)
}
