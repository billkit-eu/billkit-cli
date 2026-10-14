package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"strconv"
	"strings"

	"github.com/billkit-eu/billkit-cli/internal/api"
	"github.com/spf13/cobra"
)

func apiCmd(g *globals) *cobra.Command {
	var (
		dataPairs []string
		input     string
		idemKey   string
	)

	cmd := &cobra.Command{
		Use:   "api <METHOD> <path>",
		Short: "Make a raw authenticated API request (escape hatch)",
		Long: "Call any BillKit API route directly. Body fields are given as repeated\n" +
			"--data key=value pairs (numbers and true/false are sent unquoted), or as a\n" +
			"whole JSON object with --input, which is the only way to send the arrays and\n" +
			"nested objects several routes require (an API key's `scopes`, a webhook\n" +
			"endpoint's `enabled_events`, any resource's `metadata`).\n\n" +
			"--input takes a file holding the JSON object, or - to read it from stdin\n" +
			"(gh api's flag, with gh's meaning). The response JSON is printed to stdout.\n\n" +
			"Anything that isn't a GET carries an Idempotency-Key, so a retry can't\n" +
			"double-apply. Pass --idempotency-key to choose it yourself.\n\n" +
			"  billkit api GET /v1/customers\n" +
			"  billkit api POST /v1/customers --data email=ada@example.com --data name=Ada\n" +
			"  billkit api POST /v1/webhook_endpoints --input endpoint.json\n" +
			"  echo '{\"label\":\"ci\",\"scopes\":[\"events:read\"]}' | billkit api POST /v1/api_keys --input -",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			method := strings.ToUpper(args[0])
			path := args[1]
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}

			// Before any credential is read, for the same reason `listen`
			// checks its flags first: a mistyped invocation is the user's to
			// fix either way, and reporting it after "> Using API host …"
			// reads as though the connection is what went wrong.
			//
			// A GET/HEAD with a body is not a thing this API has: net/http
			// would send it, the server would ignore it, and the user would
			// be left reading a response that looks like their filter was
			// applied when nothing was sent at all.
			// `--input=` (or `--input ""`, an unset variable in a script)
			// used to read as no --input at all and send a bodiless call.
			if cmd.Flags().Changed("input") && input == "" {
				return usageErrorf("--input needs a file path, or - to read stdin")
			}
			if method == http.MethodGet || method == http.MethodHead {
				if flag := bodyFlagUsed(dataPairs, input); flag != "" {
					return usageErrorf("%s cannot carry a request body: drop %s, or use a method that takes one", method, flag)
				}
			}

			body, err := requestBody(cmd, dataPairs, input)
			if err != nil {
				return err
			}

			// Anything that isn't a read goes through the mutating path, so
			// the escape hatch can't quietly become the one keyless way to
			// POST /v1/refunds. The read set is the client's own
			// api.SafeMethod, so OPTIONS cannot be a read to the retry policy
			// and a money command to the confirmation gate at the same time.
			if !api.SafeMethod(method) {
				out, err := runMutating(cmd, g, mutatingCall{
					method:   method,
					path:     path,
					body:     body,
					moves:    isMoneyPath(path),
					what:     method + " " + path,
					checkCmd: "billkit api GET " + shellQuote(path),
					idemKey:  idemKey,
				})
				if err != nil {
					return err
				}
				fprintJSON(cmd.OutOrStdout(), g.color, out)
				return nil
			}

			c, err := client(cmd, g)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), readTimeout)
			defer cancel()
			out, err := c.Do(ctx, method, path, body)
			if err != nil {
				return err
			}
			fprintJSON(cmd.OutOrStdout(), g.color, out)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&dataPairs, "data", nil, "body field as key=value (repeatable; flat values only)")
	cmd.Flags().StringVar(&input, "input", "", "file holding the whole request body as a JSON object, or - for stdin")
	cmd.Flags().StringVar(&idemKey, "idempotency-key", "", "Idempotency-Key for safe retries (auto-generated when omitted)")
	// They describe the same thing two ways, and silently merging them would
	// make the precedence between them a thing anyone had to know.
	cmd.MarkFlagsMutuallyExclusive("data", "input")
	return cmd
}

// bodyFlagUsed names the flag that supplied a request body, or "" when
// neither did. The flags are mutually exclusive, so at most one can be set.
func bodyFlagUsed(dataPairs []string, input string) string {
	switch {
	case input != "":
		return "--input"
	case len(dataPairs) > 0:
		return "--data"
	default:
		return ""
	}
}

// requestBody assembles the request body from whichever flag supplied it, or
// returns nil when neither did.
//
// --data covers the flat case and cannot express anything else: its values are
// strings with a little coercion, so an array (`scopes`, `enabled_events`,
// `applies_to_price_ids`) or a nested object (`metadata`) is simply not
// sayable. Those are required fields on real routes, which left `billkit api`
// unable to reach routes it is documented as the escape hatch for. --input is
// that escape hatch's escape hatch.
func requestBody(cmd *cobra.Command, dataPairs []string, input string) (any, error) {
	if input != "" {
		return inputBody(cmd, input)
	}
	if len(dataPairs) == 0 {
		return nil, nil
	}
	body := map[string]any{}
	for _, pair := range dataPairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, usageErrorf("invalid --data %q (expected key=value)", pair)
		}
		body[key] = coerce(value)
	}
	return body, nil
}

// inputBody resolves --input and hands back the bytes to send.
//
// The flag was --json before 1.0. It was renamed because --json means
// "print JSON" on nearly every CLI that has it, this one's global flag
// included, and gh, whose `api` command this one is shaped after, calls the
// request body --input. It takes a path or `-`, as gh's does, and not inline
// JSON: a value that is sometimes a file name and sometimes a document is a
// value whose meaning depends on what happens to exist on disk.
//
// The payload travels as json.RawMessage, i.e. verbatim. Decoding it into
// map[string]any and re-encoding would turn every number into a float64 and
// silently rewrite an integer past 2^53, which is the shape of an amount in
// minor units, so "pass the body through unchanged" is a correctness
// requirement and not tidiness.
func inputBody(cmd *cobra.Command, value string) (any, error) {
	var raw []byte
	if value == "-" {
		// Reading stdin here means confirmLive cannot prompt on it. That is
		// already the right outcome: stdin is a pipe, interactive() says so,
		// and a live money call fails closed naming --yes rather than
		// blocking on a read nobody can answer.
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("could not read the request body from stdin: %w", err)
		}
		raw = data
	} else {
		// G304: reading the file the user named is what this flag is for.
		// A file that is not there or cannot be read is the invocation's
		// fault, like every other bad --input, so it exits 2.
		data, err := os.ReadFile(value) //nolint:gosec
		if err != nil {
			return nil, usageErrorf("could not read the request body: %v", err)
		}
		raw = data
	}

	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, usageErrorf("--input %s is empty", value)
	}
	if !json.Valid(raw) {
		return nil, usageErrorf("--input %s is not valid JSON", value)
	}
	// Every BillKit request body is a JSON object. Saying so here turns a
	// 400 from the server into a local error that names the problem.
	if raw[0] != '{' {
		return nil, usageErrorf("--input must hold a JSON object, got %c…", raw[0])
	}
	return json.RawMessage(raw), nil
}

// moneyPrefixes are the routes under which every write spends real money, so
// `billkit api` gets the same live-mode confirmation the first-class commands
// get.
var moneyPrefixes = []string{"/v1/refunds", "/v1/checkout"}

// moneySubscriptionActions are the sub-resource writes under
// /v1/subscriptions/{id} that can move money, verified against the services
// that implement them:
//
//   - `update` charges the prorated difference against the customer's payment
//     method inside the request.
//   - `cancel` refunds automatically when the price's `refund_on_cancel` is
//     `full` or `prorated` (services/subscriptions/lifecycle.py,
//     `_refund_on_cancel_best_effort`).
//   - `reauthorize_payment_method` creates a `sequenceType=first` Mollie
//     payment for a verification amount
//     (services/subscriptions/reauthorization.py).
//
// `pause`, `resume` and `reactivate` are deliberately absent: they take no
// payment at all.
var moneySubscriptionActions = []string{"/update", "/cancel", "/reauthorize_payment_method"}

// isMoneyPath reports whether a write to this route takes or returns money.
//
// Prefixes are not enough on their own. Three sub-resource actions under
// `/v1/subscriptions/{id}` move money and three others sharing the same
// prefix do not, so the whole prefix cannot be gated: that would put a "THIS
// MOVES REAL MONEY" banner on a pause, and a banner that cries wolf is one
// people learn to answer `y` to without reading.
//
// The path is matched in the form the server routes on, not the form it was
// typed in. `/v1/refunds/`, `/v1/%72efunds` and `/v1/x/../refunds` are all
// refunds to a server that decodes and normalises, and each used to skip the
// live-mode banner and the --yes check because the literal string did not
// start with `/v1/refunds`. Every plausible reading is checked, and anything
// that cannot be decoded counts as money: a gate that fails open on input it
// does not understand is not a gate.
func isMoneyPath(raw string) bool {
	candidates, ok := routeCandidates(raw)
	if !ok {
		return true
	}
	for _, p := range candidates {
		if matchesMoneyRoute(p) {
			return true
		}
	}
	return false
}

func matchesMoneyRoute(p string) bool {
	if strings.HasPrefix(p, "/v1/subscriptions/") {
		for _, action := range moneySubscriptionActions {
			if strings.HasSuffix(p, action) {
				return true
			}
		}
	}
	for _, prefix := range moneyPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// routeCandidates returns the readings of a typed path a server could route
// on: percent-decoded, lowercased, without a trailing slash, and both with
// and without dot segments resolved, since whether the far end resolves them
// is not something the CLI gets to know. It reports false for a path it
// cannot decode, which the caller treats as money.
//
// Decoding repeats until the path stops changing, so a double-encoded
// `%2572` is read as `r` too. One decode is all a server does, so this can
// only ever gate more than the server would route to money, never less.
// Lowercasing is the same trade: the API's routes are case-sensitive, so
// `/v1/REFUNDS` is a 404, and gating a 404 costs nothing.
func routeCandidates(raw string) ([]string, bool) {
	// `billkit api POST "/v1/refunds?x=1"` is a legal thing to type, and the
	// query has nothing to say about which route this is. Neither does a
	// fragment, which never leaves the client.
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	decoded := raw
	for i := 0; ; i++ {
		next, err := url.PathUnescape(decoded)
		if err != nil {
			return nil, false
		}
		if next == decoded {
			break
		}
		if i == 8 {
			return nil, false
		}
		decoded = next
	}
	decoded = strings.ToLower(decoded)
	literal := "/" + strings.Trim(decoded, "/")
	return []string{literal, pathpkg.Clean("/" + decoded)}, true
}

// coerce turns "true"/"false"/"null"/integers into typed JSON values, leaving
// everything else a string.
func coerce(value string) any {
	switch value {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	// Only when the text is exactly what the number prints back as. Plain
	// ParseInt accepts "01234" and "+5" and silently sends 1234 and 5, so a
	// zero-padded reference or an order number with a leading zero arrived at
	// the API as a different value than the one typed: a data-corrupting
	// convenience, and invisible in the request the user thought they made.
	if n, err := strconv.ParseInt(value, 10, 64); err == nil && strconv.FormatInt(n, 10) == value {
		return n
	}
	return value
}
