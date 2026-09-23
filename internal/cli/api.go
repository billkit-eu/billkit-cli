package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/billkit-eu/billkit-cli/internal/api"
	"github.com/spf13/cobra"
)

func apiCmd(g *globals) *cobra.Command {
	var (
		dataPairs []string
		rawBody   string
		idemKey   string
	)

	cmd := &cobra.Command{
		Use:   "api <METHOD> <path>",
		Short: "Make a raw authenticated API request (escape hatch)",
		Long: "Call any BillKit API route directly. Body fields are given as repeated\n" +
			"--data key=value pairs (numbers and true/false are sent unquoted), or as a\n" +
			"whole JSON object with --json, which is the only way to send the arrays and\n" +
			"nested objects several routes require (an API key's `scopes`, a webhook\n" +
			"endpoint's `enabled_events`, any resource's `metadata`).\n\n" +
			"--json takes the object itself, @file to read it from a file, or - for stdin.\n\n" +
			"Anything that isn't a GET carries an Idempotency-Key, so a retry can't\n" +
			"double-apply. Pass --idempotency-key to choose it yourself.\n\n" +
			"  billkit api GET /v1/customers\n" +
			"  billkit api POST /v1/customers --data email=ada@example.com --data name=Ada\n" +
			"  billkit api POST /v1/api_keys --json '{\"name\":\"ci\",\"scopes\":[\"events:read\"]}'\n" +
			"  billkit api POST /v1/webhook_endpoints --json @endpoint.json",
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
			if method == http.MethodGet || method == http.MethodHead {
				if flag := bodyFlagUsed(dataPairs, rawBody); flag != "" {
					return fmt.Errorf("%s cannot carry a request body: drop %s, or use a method that takes one", method, flag)
				}
			}

			body, err := requestBody(cmd, dataPairs, rawBody)
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
					checkCmd: "billkit api GET " + path,
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
	cmd.Flags().StringVar(&rawBody, "json", "", "the whole request body as a JSON object, @file, or - for stdin")
	cmd.Flags().StringVar(&idemKey, "idempotency-key", "", "Idempotency-Key for safe retries (auto-generated when omitted)")
	// They describe the same thing two ways, and silently merging them would
	// make the precedence between them a thing anyone had to know.
	cmd.MarkFlagsMutuallyExclusive("data", "json")
	return cmd
}

// bodyFlagUsed names the flag that supplied a request body, or "" when
// neither did. The flags are mutually exclusive, so at most one can be set.
func bodyFlagUsed(dataPairs []string, rawBody string) string {
	switch {
	case rawBody != "":
		return "--json"
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
// unable to reach routes it is documented as the escape hatch for. --json is
// that escape hatch's escape hatch.
func requestBody(cmd *cobra.Command, dataPairs []string, rawBody string) (any, error) {
	if rawBody != "" {
		return jsonBody(cmd, rawBody)
	}
	if len(dataPairs) == 0 {
		return nil, nil
	}
	body := map[string]any{}
	for _, pair := range dataPairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --data %q (expected key=value)", pair)
		}
		body[key] = coerce(value)
	}
	return body, nil
}

// jsonBody resolves --json and hands back the bytes to send.
//
// The payload travels as json.RawMessage, i.e. verbatim. Decoding it into
// map[string]any and re-encoding would turn every number into a float64 and
// silently rewrite an integer past 2^53 — which is the shape of an amount in
// minor units, so "pass the body through unchanged" is a correctness
// requirement and not tidiness.
func jsonBody(cmd *cobra.Command, value string) (any, error) {
	var raw []byte
	switch {
	case value == "-":
		// Reading stdin here means confirmLive cannot prompt on it. That is
		// already the right outcome: stdin is a pipe, interactive() says so,
		// and a live money call fails closed naming --yes rather than
		// blocking on a read nobody can answer.
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("could not read the request body from stdin: %w", err)
		}
		raw = data
	case strings.HasPrefix(value, "@"):
		data, err := os.ReadFile(value[1:])
		if err != nil {
			return nil, fmt.Errorf("could not read the request body: %w", err)
		}
		raw = data
	default:
		raw = []byte(value)
	}

	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("--json was given an empty body")
	}
	if !json.Valid(raw) {
		return nil, errors.New("--json is not valid JSON")
	}
	// Every BillKit request body is a JSON object. Saying so here turns a
	// 422 from the server into a local error that names the problem.
	if raw[0] != '{' {
		return nil, fmt.Errorf("--json must be a JSON object, got %c…", raw[0])
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
func isMoneyPath(path string) bool {
	// `billkit api POST "/v1/refunds?x=1"` is a legal thing to type, and the
	// query has nothing to say about which route this is.
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if strings.HasPrefix(path, "/v1/subscriptions/") {
		for _, action := range moneySubscriptionActions {
			if strings.HasSuffix(path, action) {
				return true
			}
		}
	}
	for _, prefix := range moneyPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
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
	// the API as a different value than the one typed — a data-corrupting
	// convenience, and invisible in the request the user thought they made.
	if n, err := strconv.ParseInt(value, 10, 64); err == nil && strconv.FormatInt(n, 10) == value {
		return n
	}
	return value
}
