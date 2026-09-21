package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/billkit-eu/billkit-cli/internal/api"
	"github.com/spf13/cobra"
)

// mutatingCall is a request that changes state on the server, so it must
// never be replayed blindly and must never go out keyless.
type mutatingCall struct {
	method string
	path   string
	body   any

	// moves marks a call that spends real money in live mode. Those get the
	// loud banner and, in live mode, an explicit confirmation.
	moves bool
	// what describes the effect in one clause, e.g.
	// `refund 500 cents of pay_123`. Used in the banner and the warnings.
	what string
	// checkCmd is the command that shows whether the call already happened,
	// printed before a retry is suggested.
	checkCmd string
	// idemKey is the user-supplied --idempotency-key; empty means mint one.
	idemKey string
}

// runMutating performs a state-changing call with the safety rails a money
// CLI needs: an idempotency key that is always present and always shown, an
// explicit statement of which mode is about to be used, a confirmation gate
// in live mode, and honest guidance when the outcome is unknown.
//
// Everything it prints goes to stderr, so --print-json style piping and
// scripted output stay machine-readable on stdout.
func runMutating(cmd *cobra.Command, g *globals, call mutatingCall) ([]byte, error) {
	c, cr, err := clientWithTimeout(cmd, g, writeTimeout)
	if err != nil {
		return nil, err
	}
	errw := cmd.ErrOrStderr()

	key := call.idemKey
	generated := false
	if key == "" {
		if key, err = api.NewIdempotencyKey(); err != nil {
			return nil, err
		}
		generated = true
	}

	announceMode(errw, cr, call, g.color)
	announceKey(errw, key, generated)
	if err := confirmLive(cmd, g, cr, call); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), writeTimeout)
	defer cancel()
	out, err := c.Do(ctx, call.method, call.path, call.body, api.WithIdempotencyKey(key))
	if err != nil {
		var apiErr *api.APIError
		if !errors.As(err, &apiErr) {
			// No HTTP status ever came back, and the client already retried
			// with this key. The server may still have done the work, so say
			// so instead of letting the user guess.
			warnAmbiguous(errw, call, key)
		}
		return nil, err
	}
	return out, nil
}

// announceMode states which credentials are about to be used. In live mode on
// a money command it is a banner rather than a line, because "the CLI said
// test mode and then spent real money" is the failure this prevents.
func announceMode(w io.Writer, cr creds, call mutatingCall, colorMode string) {
	mode := cr.mode
	if mode == "" {
		mode = "unknown-mode"
	}
	// Name the identity, not just the host. A live-mode banner that says only
	// the URL leaves the user to guess which of --api-key, BILLKIT_API_KEY and
	// the stored profile actually won, which is the question they need
	// answered before they confirm a real charge.
	where := cr.baseURL
	if origin := cr.origin(); origin != "" {
		where = fmt.Sprintf("%s at %s", origin, cr.baseURL)
	}

	if call.moves && mode == "live" {
		alert := func(s string) string {
			if !colorEnabled(w, colorMode) {
				return s
			}
			return "\x1b[1;97;41m" + s + ansiReset
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, alert("  LIVE MODE — THIS MOVES REAL MONEY  "))
		fmt.Fprintf(w, "  Using %s\n", where)
		fmt.Fprintf(w, "  About to %s\n\n", call.what)
		return
	}
	fmt.Fprintf(w, "> %s mode (%s): %s\n", mode, where, call.what)
}

// announceKey prints the key before the call, not after. An auto-generated
// key only protects the user if the retry carries the same one, and they
// cannot copy a key they were never shown.
func announceKey(w io.Writer, key string, generated bool) {
	if generated {
		fmt.Fprintf(w, "> Idempotency-Key: %s (auto-generated; reuse it if you retry)\n", key)
		return
	}
	fmt.Fprintf(w, "> Idempotency-Key: %s\n", key)
}

// confirmLive makes live money movement a deliberate act. On a terminal it
// asks. Anywhere else (CI, a pipe, cron) it refuses and names the flag,
// because a prompt nobody can answer hangs the pipeline.
func confirmLive(cmd *cobra.Command, g *globals, cr creds, call mutatingCall) error {
	if !call.moves || cr.mode != "live" || g.yes {
		return nil
	}
	errw := cmd.ErrOrStderr()

	if !interactive(cmd) {
		return fmt.Errorf("live mode needs confirmation and stdin is not a terminal — re-run with --yes to authorise this %s", call.what)
	}
	fmt.Fprint(errw, "Proceed? [y/N]: ")
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && answer == "" {
		return fmt.Errorf("could not read a confirmation — re-run with --yes to authorise this %s", call.what)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	default:
		return errors.New("aborted")
	}
}

// interactive reports whether there is a human on the other end of stdin.
// It is deliberately conservative: when in doubt, confirmLive fails closed
// with a message naming --yes rather than blocking on a read.
func interactive(cmd *cobra.Command) bool {
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || in != os.Stdin {
		return false
	}
	info, err := in.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// warnAmbiguous is the honest version of a bare "context deadline exceeded":
// the request may well have succeeded on the server, so say what to check and
// give the exact command that retries without spending twice.
func warnAmbiguous(w io.Writer, call mutatingCall, key string) {
	fmt.Fprintf(w, "\n! No response came back, so the server may already have processed this.\n")
	fmt.Fprintf(w, "!   The %s may already exist.\n", call.what)
	if call.checkCmd != "" {
		fmt.Fprintf(w, "!   Check first:  %s\n", call.checkCmd)
	}
	fmt.Fprintf(w, "!   Then retry safely with the same key:\n!     %s\n", retryCommand(key))
}

// retryCommand renders the command the user just ran, carrying the
// idempotency key, so a retry is deduplicated rather than charged again.
func retryCommand(key string) string {
	args := make([]string, 0, len(os.Args)+2)
	if len(os.Args) > 0 {
		args = append(args, filepath.Base(os.Args[0]))
		args = append(args, os.Args[1:]...)
	} else {
		args = append(args, "billkit")
	}
	if !hasIdempotencyArg(args) {
		args = append(args, "--idempotency-key", key)
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// hasIdempotencyArg reports whether the command line already carries the key,
// in either --flag value or --flag=value form.
func hasIdempotencyArg(args []string) bool {
	for _, a := range args {
		if a == "--idempotency-key" || strings.HasPrefix(a, "--idempotency-key=") {
			return true
		}
	}
	return false
}

// shellQuote makes an argument safe to paste back into a shell.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\$`|&;<>()*?[]{}#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
