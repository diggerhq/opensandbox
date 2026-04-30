package secrets

import (
	"encoding/json"
	"fmt"
)

// parseBundleJSON returns secrets[key] from a JSON-encoded object string.
// Used by Backend implementations that store multiple secrets per ARN/version.
func parseBundleJSON(s, key string) (string, error) {
	var bundle map[string]string
	if err := json.Unmarshal([]byte(s), &bundle); err != nil {
		return "", fmt.Errorf("secrets: bundle JSON parse: %w", err)
	}
	v, ok := bundle[key]
	if !ok {
		return "", fmt.Errorf("secrets: %w (key %q not in bundle)", ErrNotFound, key)
	}
	return v, nil
}
