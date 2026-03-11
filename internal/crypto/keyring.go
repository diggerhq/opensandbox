package crypto

import (
	"encoding/binary"
	"fmt"
)

// KeyRing holds multiple versioned encryption keys. New data is always encrypted
// with the primary (highest-version) key; decryption tries the key matching the
// version stored in the ciphertext header.
//
// Ciphertext format: [2-byte big-endian version] [nonce] [AES-256-GCM ciphertext]
//
// Configuration:
//
//	OPENSANDBOX_SECRET_ENCRYPTION_KEY    — primary key (hex, required)
//	OPENSANDBOX_SECRET_ENCRYPTION_KEY_V1 — previous key for rotation (hex, optional)
//	OPENSANDBOX_SECRET_ENCRYPTION_KEY_V2 — etc.
type KeyRing struct {
	primary        *Encryptor
	primaryVersion uint16
	keys           map[uint16]*Encryptor
}

// NewKeyRing creates a KeyRing from versioned keys. The map must contain at least
// one key. The highest version is used for encryption.
func NewKeyRing(keys map[uint16]string) (*KeyRing, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("keyring: at least one key is required")
	}

	ring := &KeyRing{keys: make(map[uint16]*Encryptor, len(keys))}
	for ver, hexKey := range keys {
		enc, err := NewEncryptor(hexKey)
		if err != nil {
			return nil, fmt.Errorf("keyring: key v%d: %w", ver, err)
		}
		ring.keys[ver] = enc
		if ring.primary == nil || ver > ring.primaryVersion {
			ring.primary = enc
			ring.primaryVersion = ver
		}
	}
	return ring, nil
}

// Encrypt encrypts data with the primary key, prepending the version header.
func (r *KeyRing) Encrypt(plaintext []byte) ([]byte, error) {
	inner, err := r.primary.Encrypt(plaintext)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 2+len(inner))
	binary.BigEndian.PutUint16(out[:2], r.primaryVersion)
	copy(out[2:], inner)
	return out, nil
}

// Decrypt reads the version header and decrypts with the matching key.
// Falls back to the primary key for legacy data without a version header
// (ciphertext shorter than expected or version 0 with no v0 key registered).
func (r *KeyRing) Decrypt(data []byte) ([]byte, error) {
	// Legacy unversioned data: try primary key directly
	if len(data) < 2 {
		return r.primary.Decrypt(data)
	}

	ver := binary.BigEndian.Uint16(data[:2])
	if enc, ok := r.keys[ver]; ok {
		result, err := enc.Decrypt(data[2:])
		if err == nil {
			return result, nil
		}
		// If versioned decrypt fails, fall through to legacy
	}

	// Legacy fallback: data may not have a version header (pre-rotation secrets).
	// Try primary key on the full blob.
	return r.primary.Decrypt(data)
}

// PrimaryVersion returns the version number of the active encryption key.
func (r *KeyRing) PrimaryVersion() uint16 {
	return r.primaryVersion
}

// AsEncryptor returns an Encryptor-compatible wrapper so the KeyRing can be
// used anywhere an Encryptor is expected without changing call sites.
func (r *KeyRing) AsEncryptor() *Encryptor {
	return &Encryptor{gcm: r.primary.gcm, keyRing: r}
}
