package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// The API's page-size bounds for every list route (`limit`: 1 to 100).
const (
	minLimit = 1
	maxLimit = 100
)

// limitUsage is the --limit help text, so every list command words it alike.
func limitUsage(what string) string {
	return fmt.Sprintf("max %s to return, %d to %d (default: the API's, 10)", what, minLimit, maxLimit)
}

// setLimit validates --limit and adds it to the query when it was given.
//
// An out-of-range value is a bad invocation. It used to be dropped silently
// when it was zero or negative, so `--limit -5` returned a page of ten and
// read as though the flag had worked; and above 100 it was sent and came
// back as a 400 naming a query parameter rather than the flag.
func setLimit(cmd *cobra.Command, q url.Values, limit int) error {
	if !cmd.Flags().Changed("limit") {
		return nil
	}
	if limit < minLimit || limit > maxLimit {
		return usageErrorf("--limit must be between %d and %d, got %d", minLimit, maxLimit, limit)
	}
	q.Set("limit", strconv.Itoa(limit))
	return nil
}

// withQuery appends an encoded query to a path, if there is one.
func withQuery(path string, q url.Values) string {
	if encoded := q.Encode(); encoded != "" {
		return path + "?" + encoded
	}
	return path
}

// getAndPrint performs one authenticated read and prints the JSON body.
func getAndPrint(cmd *cobra.Command, g *globals, path string) error {
	c, err := client(cmd, g)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), readTimeout)
	defer cancel()
	out, err := c.Do(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	fprintJSON(cmd.OutOrStdout(), g.color, out)
	return nil
}

// idOf extracts the top-level "id" from a JSON object body.
func idOf(raw []byte) string {
	var obj struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.ID
}

// parseMetadata turns repeated key=value pairs into a string map for the API's
// `metadata` field.
func parseMetadata(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid --metadata %q (expected key=value)", pair)
		}
		out[key] = value
	}
	return out, nil
}
