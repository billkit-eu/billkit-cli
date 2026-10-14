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
	"os"
	"slices"
	"strings"
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
var Version = "1.0.0"

// Commit and Date describe the build, for `billkit version`. GoReleaser sets
// both by ldflags (the full commit hash, and the commit's own timestamp so a
// rebuild of the same tag is byte-identical). A build without them falls back
// to what the Go toolchain stamped from the VCS, see buildInfo.
var (
	Commit = ""
	Date   = ""
)

// globals holds the values behind the root command's persistent flags.
//
// One instance is created by rootCmd() and handed to every subcommand
// constructor. Package-level flag variables would be simpler to write and
// wrong in the two ways that matter: two command trees in one process
// (tests, an embedding caller) would share them, and a test that set one
// would leak it into the next test unless it remembered to restore it,
// which is exactly the bug class a CLI that reads credentials should not
// be exposed to.
type globals struct {
	profile string
	apiKey  string
	baseURL string
	color   string
	yes     bool
	json    bool

	// announcedHost keeps the non-default-host notice to one line per
	// command tree. It was a package-level sync.Once, which meant a second
	// tree in the same process stayed silent about a host the first one had
	// already named, and that notice exists precisely so a repointed CLI
	// cannot carry a key somewhere quietly.
	announcedHost bool
}

// notices is where a command's informational messages go: w (stderr), or
// nowhere under --json, where stderr is the machine channel and carries the
// error envelope and nothing else a script would have to skip. Warnings that
// need a person (the `!` lines, the live-mode banner and its prompt, the
// outcome-unknown guidance) are written to stderr regardless, before the
// envelope, so the envelope is always the last line.
func (g *globals) notices(w io.Writer) io.Writer {
	if g.json {
		return io.Discard
	}
	return w
}

// Deadlines. Reads are cheap to repeat, so they stay short. Money-moving
// writes get far longer: the API makes a live Mollie round trip inside the
// request, and a client that gives up while the provider is still working is
// what manufactures the "did it happen?" ambiguity in the first place.
const (
	readTimeout  = 30 * time.Second
	writeTimeout = 80 * time.Second
)

// colorModes are the values --color accepts.
var colorModes = []string{"auto", "always", "never"}

func rootCmd() *cobra.Command {
	root, _ := newRoot()
	return root
}

// newRoot builds the command tree and hands back the globals behind its
// persistent flags, which Main needs to render an error the way --json asked.
func newRoot() (*cobra.Command, *globals) {
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
			"  billkit one-shots create --customer cus_1 --amount 1999 --payment-method ideal \\\n" +
			"      --success-url https://example.com/thanks",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
		// Checked before any command runs, so a typo in a global flag is a
		// bad invocation (exit 2) everywhere, not something each command has
		// to remember to look at. --color used to accept any string and
		// quietly treat it as auto.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if !slices.Contains(colorModes, g.color) {
				return usageErrorf("invalid --color %q: expected one of %s", g.color, strings.Join(colorModes, ", "))
			}
			// cobra checks required flags and mutually exclusive groups
			// itself, right after this hook, and returns its errors
			// untagged, so `one-shots create` without --customer exited 1
			// as though a call had failed. Running the same two checks
			// here first is what makes them exit 2; cobra's own pass then
			// finds nothing.
			if err := cmd.ValidateRequiredFlags(); err != nil {
				return &UsageError{err}
			}
			if err := cmd.ValidateFlagGroups(); err != nil {
				return &UsageError{err}
			}
			return nil
		},
	}

	root.PersistentFlags().StringVar(&g.profile, "profile", "", "config profile to use (env: BILLKIT_PROFILE; default: the configured default)")
	root.PersistentFlags().StringVar(&g.apiKey, "api-key", "", "API key for this call; prefer BILLKIT_API_KEY, since a flag value is visible in the process list and in shell history")
	root.PersistentFlags().StringVar(&g.baseURL, "base-url", "", "API host override (env: BILLKIT_BASE_URL; default: https://api.billkit.eu)")
	root.PersistentFlags().StringVar(&g.color, "color", "auto", "colorize JSON output: auto, always, or never")
	root.PersistentFlags().BoolVarP(&g.yes, "yes", "y", false, "confirm live-mode money commands without prompting (required in scripts)")
	root.PersistentFlags().BoolVar(&g.json, "json", false, "machine-readable output: listings print JSON, and errors are written to stderr as a JSON object")
	// Declared here, with no shorthand, so cobra does not add its default
	// `-v`. -v is left free on purpose: gh spells version --version only,
	// and a 1.0 that gave -v to version could never give it to --verbose.
	root.Flags().Bool("version", false, "print the version, commit and build date")
	// The same line `billkit version` prints. It holds no template actions,
	// so cobra's template engine passes it through untouched.
	root.SetVersionTemplate(versionLine() + "\n")

	root.AddCommand(
		loginCmd(g),
		logoutCmd(g),
		configCmd(g),
		listenCmd(g),
		triggerCmd(g),
		eventsCmd(g),
		refundsCmd(g),
		oneShotsCmd(g),
		apiCmd(g),
		versionCmd(g),
	)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &UsageError{err} })
	// cobra adds these two lazily, inside Execute, which is after the walks
	// below would have seen them. `completion` is a group like any other, so
	// `billkit completion bogus` would otherwise still exit 0.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	refuseUnknownSubcommands(root)
	refuseUnknownHelpTopics(root)
	root.SetUsageTemplate(groupAwareUsageTemplate(root.UsageTemplate()))
	markUsageErrors(root)
	return root, g
}

// refuseUnknownSubcommands makes every group command (one with subcommands and
// no action of its own, the root included) reject an argument that names no
// subcommand, as a UsageError.
//
// cobra does not, and the gap is in two different places. A group below the
// root accepts any arguments and, having nothing to run, prints its help and
// returns nil, so `billkit refunds notasub` exited 0, which a script reads as
// success. The root does refuse an unknown name, but from inside Find, before
// any Args validator runs, so `billkit notacommand` was an untagged error and
// exited 1, the status that means "the call ran and failed".
//
// A bare group with no arguments still prints its help and exits 0. Asking
// `billkit refunds` what it can do is a request for help, not a mistake, and
// that is what git, gh and the Stripe CLI all do with it.
func refuseUnknownSubcommands(c *cobra.Command) {
	if c.HasSubCommands() && c.Run == nil && c.RunE == nil {
		c.Args = unknownSubcommand
		c.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		c.Annotations[groupAnnotation] = "true"
	}
	for _, sub := range c.Commands() {
		refuseUnknownSubcommands(sub)
	}
}

// groupAnnotation marks a command refuseUnknownSubcommands made runnable only
// so it could validate its arguments. The usage template reads it.
const groupAnnotation = "billkit:group"

// groupAwareUsageTemplate is cobra's usage template with one change: a group
// does not print its own `billkit refunds [flags]` line. cobra prints that
// line for any runnable command, and a group is runnable only because of the
// RunE above, so without this every group's usage read as though it took
// flags and ran on its own, above the `[command]` line that is the real
// answer. A template that does not contain the expected line is returned
// unchanged, so a cobra upgrade that rewords it degrades to the extra line
// rather than to broken help.
func groupAwareUsageTemplate(tmpl string) string {
	const runnable = "{{if .Runnable}}\n  {{.UseLine}}{{end}}"
	const notGroup = "{{if and .Runnable (ne (index .Annotations \"" + groupAnnotation + "\") \"true\")}}\n  {{.UseLine}}{{end}}"
	return strings.Replace(tmpl, runnable, notGroup, 1)
}

// refuseUnknownHelpTopics makes `billkit help bogus` a UsageError. cobra's help
// command resolves its topic with Find, and since the root has an Args
// validator Find hands an unknown name back as a leftover argument of the
// root rather than as an error, so the help command printed the root's help
// and exited 0. The validator is declared here, before markUsageErrors, so
// the refusal exits 2 like any other bad invocation.
func refuseUnknownHelpTopics(root *cobra.Command) {
	for _, c := range root.Commands() {
		if c.Name() != "help" {
			continue
		}
		c.Args = func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			target, rest, err := cmd.Root().Find(args)
			if err == nil && target != nil && len(rest) == 0 {
				return nil
			}
			// Worded like unknownSubcommand's message, which ends the same way.
			msg := fmt.Sprintf("unknown help topic %q\n\nRun '%s --help' for the available commands.",
				strings.Join(args, " "), cmd.Root().CommandPath())
			return errors.New(msg)
		}
	}
}

// unknownSubcommand is a group's argument validator. It words the error the
// way cobra's own root check does, suggestions included, and points at the
// group's help, since that is where the valid names are.
func unknownSubcommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	// SuggestionsFor reads this as "no suggestions at all" while it is zero;
	// cobra's own check sets the same default before calling it.
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t")
	}
	msg += fmt.Sprintf("\n\nRun '%s --help' for the available commands.", cmd.CommandPath())
	return errors.New(msg)
}

// Process exit statuses. A script wrapping this CLI has to be able to tell
// "I typed the command wrong" from "the command ran and the call failed" from
// "I am not logged in", because each wants a different reaction: fix the
// command, maybe retry, or go and get a credential.
const (
	// ExitFailure is a command that ran and did not succeed: an API error,
	// a network failure, a refused transport, a declined confirmation.
	ExitFailure = 1
	// ExitUsage is a bad invocation: an unknown command or flag, a malformed
	// or out-of-range flag value, the wrong number of arguments, an unknown
	// profile name, or a live-mode money command run without a terminal and
	// without --yes. It is the conventional 2, the same one getopt-based
	// tools and `go` itself use.
	ExitUsage = 2
	// ExitAuth is a missing or rejected credential: no key configured or
	// supplied, a keychain entry that cannot be read, or an API key the
	// server answered 401 to. 4 is gh's code for the same condition.
	ExitAuth = 4
)

// UsageError marks an error as a bad invocation rather than a failed call.
// It is a wrapper rather than a sentinel so the original message, which cobra
// and pflag word perfectly well, is what the user still reads.
type UsageError struct{ err error }

func (e *UsageError) Error() string { return e.err.Error() }
func (e *UsageError) Unwrap() error { return e.err }

// usageErrorf builds a UsageError from a format string.
func usageErrorf(format string, args ...any) error {
	return &UsageError{fmt.Errorf(format, args...)}
}

// AuthError marks an error as "no usable credential". Like UsageError it
// wraps, so the message the user reads is unchanged.
type AuthError struct{ err error }

func (e *AuthError) Error() string { return e.err.Error() }
func (e *AuthError) Unwrap() error { return e.err }

// ExitCode maps an error returned by Execute onto a process exit status.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var usage *UsageError
	if errors.As(err, &usage) {
		return ExitUsage
	}
	if isAuthFailure(err) {
		return ExitAuth
	}
	return ExitFailure
}

// isAuthFailure reports whether err means the CLI has no credential the API
// will accept: one this CLI marked as such, or a 401 from any of the three
// clients (a call, the `listen` stream, or `login`'s own check).
func isAuthFailure(err error) bool {
	var auth *AuthError
	if errors.As(err, &auth) {
		return true
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		return true
	}
	var stream *streamStatusError
	return errors.As(err, &stream) && stream.status == http.StatusUnauthorized
}

// markUsageErrors tags the two error paths cobra has for a bad invocation.
// It runs after refuseUnknownSubcommands, so a group's unknown-subcommand
// check is wrapped here like any other validator.
// Flag parsing is one hook for the whole tree (FlagErrorFunc is inherited
// from the root), and argument validation is per-command, so it is wrapped
// where a command actually declares a validator. A command that declares none
// is left alone rather than given cobra's default, because substituting a
// validator changes which errors it produces.
func markUsageErrors(c *cobra.Command) {
	if inner := c.Args; inner != nil {
		c.Args = func(cmd *cobra.Command, args []string) error {
			if err := inner(cmd, args); err != nil {
				return &UsageError{err}
			}
			return nil
		}
	}
	for _, sub := range c.Commands() {
		markUsageErrors(sub)
	}
}

// Execute runs the CLI and returns its error without printing it.
func Execute() error {
	return rootCmd().Execute()
}

// Main runs the CLI with the process's arguments, prints any error to stderr
// in the format the output contract promises, and returns the exit status.
func Main() int {
	root, g := newRoot()
	err := root.Execute()
	if err == nil {
		return 0
	}
	// --json may not have been parsed when the error is a flag error that
	// came before it, so the raw arguments are consulted too.
	asJSON := g.json || slices.ContainsFunc(os.Args[1:], func(a string) bool { return a == "--json" || a == "--json=true" })
	writeError(root.ErrOrStderr(), err, asJSON)
	return ExitCode(err)
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
			switch {
			case errors.Is(err, config.ErrNoProfile):
				return creds{}, &AuthError{fmt.Errorf("%w, or set %s, or pass --api-key", err, envAPIKey)}
			case errors.Is(err, config.ErrUnknownProfile):
				return creds{}, &UsageError{err}
			case errors.Is(err, config.ErrKeychainRead):
				return creds{}, &AuthError{err}
			}
			return creds{}, err
		}
		out = creds{apiKey: profile.APIKey, baseURL: profile.BaseURL, profile: name}
		if baseURL != "" {
			out.baseURL = baseURL
		}
	}

	// The same check `login` makes, applied to every tier and every command,
	// so a key that is not BillKit's (a Stripe `sk_…` pasted into the wrong
	// variable, say) is refused here and never sent to any host. It used to
	// reach the API, which answered 401, so the exit code was right but a
	// foreign secret had already gone over the wire.
	if out.apiKey != "" && config.Mode(out.apiKey) == "" {
		return creds{}, &AuthError{fmt.Errorf(
			"the API key from %s doesn't look like a BillKit key (expected bk_test_… or bk_live_…), so it was not sent",
			out.origin())}
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
		return nil, creds{}, errNoAPIKey()
	}
	announceHost(g.notices(cmd.ErrOrStderr()), g, cr)
	// The transport cap sits just above the per-call context deadline so the
	// context is what fires, and the error names the deadline the user can
	// reason about.
	return api.New(cr.baseURL, cr.apiKey, Version, newHTTPClient(timeout+5*time.Second)), cr, nil
}

// errNoAPIKey is the error for an invocation with no key from any tier.
func errNoAPIKey() error {
	return &AuthError{fmt.Errorf("no API key: run `billkit login`, or set %s (preferred in scripts), or pass --api-key", envAPIKey)}
}

// transportOverride is a test seam. It lets the package's own tests trust an
// httptest TLS server's self-signed certificate without loosening any of the
// transport rules the CLI enforces. It is nil in every shipped build.
var transportOverride http.RoundTripper

// newHTTPClient builds the client for one authenticated API call. It follows
// a redirect only under api.FollowSafeRedirects (https only, Authorization
// dropped on any host change); api.New enforces the same policy again, so a
// caller that bypasses this constructor is still covered.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: transportOverride, CheckRedirect: api.FollowSafeRedirects}
}

// announceHost says out loud, once per command tree, when the CLI is pointed
// somewhere other than the official API. A repointed CLI should announce
// itself rather than quietly carry the key off to a host the user did not
// expect.
//
// It writes to the command's own error stream, not os.Stderr, so the notice
// lands wherever the caller redirected the command, which is also what makes
// it assertable without swapping a package variable out from under the test.
func announceHost(w io.Writer, g *globals, cr creds) {
	if cr.isDefaultHost() || g.announcedHost {
		return
	}
	g.announcedHost = true
	fmt.Fprintf(w, "> Using API host %s\n", cr.baseURL)
}
