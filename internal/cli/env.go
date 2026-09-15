package cli

import (
	"os"
	"strings"
)

// Environment variables the CLI reads, alongside BILLKIT_CONFIG_HOME (which
// internal/config owns) and NO_COLOR (output.go).
//
// BILLKIT_API_KEY exists because --api-key used to be the only non-interactive
// way to hand the CLI a credential, and a flag value is the worst place to put
// a secret: argv is world-readable in /proc on Linux, it shows up in `ps auxww`
// for the life of the call, CI logs echo the command line, and an interactive
// shell appends it verbatim to ~/.zsh_history. A variable is scoped to this
// process and its children and never reaches any of those.
//
// BILLKIT_BASE_URL and BILLKIT_PROFILE come along with it so an environment can
// describe a whole identity -- which key, which host, which stored profile --
// without any of the three needing a flag. That also matches every BillKit
// SDK, which already resolves BILLKIT_API_KEY and BILLKIT_BASE_URL, so a
// container that can run the Python SDK can run the CLI unchanged.
const (
	envAPIKey  = "BILLKIT_API_KEY"
	envBaseURL = "BILLKIT_BASE_URL"
	envProfile = "BILLKIT_PROFILE"
)

// Precedence for all three is: flag, then environment, then the stored
// profile.
//
// The flag wins because it is the most deliberate signal available. Someone
// typed it on this invocation, knowing what they were running; an exported
// variable may have been sitting in the shell since a login an hour ago, and a
// tool that let that quietly beat what the user just typed would be impossible
// to override in the one case where overriding matters. The environment beats
// the stored profile for the mirror-image reason: it is scoped to this process
// tree, so it says "this run is that identity" without writing a live key to
// the runner's disk, and a CI job should not have to log in to use a key it
// already holds.
//
// An environment-supplied key is not trusted any further than a flag-supplied
// one. Both land in resolve(), so both go through config.CheckTransport before
// a byte reaches the wire: a live key from BILLKIT_API_KEY pointed at a plain
// http host is refused exactly as `--api-key bk_live_… --base-url http://…`
// is.

// envValue reads an environment variable and trims surrounding whitespace.
// Keys arrive out of `$(cat secret)`, a Kubernetes secret file, or a CI
// masking step with a trailing newline more often than not, and a newline
// inside an Authorization header is a protocol error rather than a 401 the
// user can read.
func envValue(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// apiKeyOverride returns the explicitly supplied API key and the name of where
// it came from ("--api-key" or "BILLKIT_API_KEY"), or two empty strings when
// neither was given and the stored profile should be used.
//
// The source is a label only. The key itself is never logged, echoed, or put
// into an error message.
func apiKeyOverride() (key, source string) {
	if flagAPIKey != "" {
		return flagAPIKey, "--api-key"
	}
	if key := envValue(envAPIKey); key != "" {
		return key, envAPIKey
	}
	return "", ""
}

// baseURLOverride returns the API host override, or "" to leave the choice to
// the stored profile.
func baseURLOverride() string {
	if flagBaseURL != "" {
		return flagBaseURL
	}
	return envValue(envBaseURL)
}

// profileOverride returns the stored profile to use, or "" for the default.
func profileOverride() string {
	if flagProfile != "" {
		return flagProfile
	}
	return envValue(envProfile)
}
