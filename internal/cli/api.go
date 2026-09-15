package cli

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func apiCmd() *cobra.Command {
	var (
		dataPairs []string
		idemKey   string
	)

	cmd := &cobra.Command{
		Use:   "api <METHOD> <path>",
		Short: "Make a raw authenticated API request (escape hatch)",
		Long: "Call any BillKit API route directly. Body fields are given as repeated\n" +
			"--data key=value pairs (numbers and true/false are sent unquoted).\n\n" +
			"Anything that isn't a GET carries an Idempotency-Key, so a retry can't\n" +
			"double-apply. Pass --idempotency-key to choose it yourself.\n\n" +
			"  billkit api GET /v1/customers\n" +
			"  billkit api POST /v1/customers --data email=ada@example.com --data name=Ada",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			method := strings.ToUpper(args[0])
			path := args[1]
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}

			var body map[string]any
			if len(dataPairs) > 0 {
				body = map[string]any{}
				for _, pair := range dataPairs {
					key, value, ok := strings.Cut(pair, "=")
					if !ok {
						return fmt.Errorf("invalid --data %q (expected key=value)", pair)
					}
					body[key] = coerce(value)
				}
			}

			// Anything that isn't a read goes through the mutating path, so
			// the escape hatch can't quietly become the one keyless way to
			// POST /v1/refunds.
			if method != http.MethodGet && method != http.MethodHead {
				out, err := runMutating(cmd, mutatingCall{
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
				printJSON(out)
				return nil
			}

			c, err := client()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), readTimeout)
			defer cancel()
			out, err := c.Do(ctx, method, path, body)
			if err != nil {
				return err
			}
			printJSON(out)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&dataPairs, "data", nil, "body field as key=value (repeatable)")
	cmd.Flags().StringVar(&idemKey, "idempotency-key", "", "Idempotency-Key for safe retries (auto-generated when omitted)")
	return cmd
}

// moneyPaths are the routes where a replay spends real money, so `billkit api`
// gets the same live-mode confirmation the first-class commands get.
var moneyPaths = []string{"/v1/refunds", "/v1/checkout"}

func isMoneyPath(path string) bool {
	for _, prefix := range moneyPaths {
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
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return n
	}
	return value
}
