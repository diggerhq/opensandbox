package secrets

import (
	"encoding/json"
	"fmt"
)

// parseBundleJSON returns secrets[key] from a JSON-encoded object string.
// Used by Backend implementations that store multiple secrets per ARN/version.
func parseBundleJSON(s, key string) (string, error) {
	bundle, err := parseBundleJSONAll(s)
	if err != nil {
		return "", err
	}
	v, ok := bundle[key]
	if !ok {
		return "", fmt.Errorf("secrets: %w (key %q not in bundle)", ErrNotFound, key)
	}
	return v, nil
}

// parseBundleJSONAll returns the full {key: value} map from a JSON-encoded
// object string. Used by BulkLoader implementations that copy every entry
// into the process environment.
func parseBundleJSONAll(s string) (map[string]string, error) {
	var bundle map[string]string
	if err := json.Unmarshal([]byte(s), &bundle); err != nil {
		return nil, fmt.Errorf("secrets: bundle JSON parse: %w", err)
	}
	return bundle, nil
}
