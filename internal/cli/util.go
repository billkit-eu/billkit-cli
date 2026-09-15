package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

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
