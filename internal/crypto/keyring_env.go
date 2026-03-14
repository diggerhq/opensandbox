package crypto

import (
	"fmt"
	"os"
)

// NewKeyRingFromEnv builds a KeyRing from environment variables:
//
//	OPENSANDBOX_SECRET_ENCRYPTION_KEY    — primary key (version 0 if no _V* keys exist)
//	OPENSANDBOX_SECRET_ENCRYPTION_KEY_V1 — version 1
//	OPENSANDBOX_SECRET_ENCRYPTION_KEY_V2 — version 2 (highest = primary)
//	...up to V9
//
// Returns nil if no key is configured.
func NewKeyRingFromEnv() (*KeyRing, error) {
	baseKey := os.Getenv("OPENSANDBOX_SECRET_ENCRYPTION_KEY")
	if baseKey == "" {
		return nil, nil
	}

	keys := make(map[uint16]string)

	// Check for versioned keys V1..V9
	hasVersioned := false
	for v := uint16(1); v <= 9; v++ {
		envKey := fmt.Sprintf("OPENSANDBOX_SECRET_ENCRYPTION_KEY_V%d", v)
		if hexKey := os.Getenv(envKey); hexKey != "" {
			keys[v] = hexKey
			hasVersioned = true
		}
	}

	if hasVersioned {
		// If versioned keys exist, the base key is used as the fallback for
		// legacy (unversioned) data. Assign it as version 0 so KeyRing.Decrypt
		// can find it for legacy blobs, but the highest V* key becomes primary.
		keys[0] = baseKey
	} else {
		// No versioned keys — single-key mode. Version 1.
		keys[1] = baseKey
	}

	return NewKeyRing(keys)
}
