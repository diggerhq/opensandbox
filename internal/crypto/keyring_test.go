package crypto

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func randomHexKey() string {
	key := make([]byte, 32)
	rand.Read(key)
	return hex.EncodeToString(key)
}

func TestKeyRingEncryptDecrypt(t *testing.T) {
	ring, err := NewKeyRing(map[uint16]string{
		1: randomHexKey(),
	})
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("super-secret-api-key")
	ct, err := ring.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ring.Decrypt(ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("got %q, want %q", got, plaintext)
	}
}

func TestKeyRingRotation(t *testing.T) {
	oldKey := randomHexKey()
	newKey := randomHexKey()

	// Encrypt with old key (v1)
	oldRing, _ := NewKeyRing(map[uint16]string{1: oldKey})
	ct, _ := oldRing.Encrypt([]byte("old-secret"))

	// Create new ring with both keys (v2 is primary)
	newRing, _ := NewKeyRing(map[uint16]string{
		1: oldKey,
		2: newKey,
	})

	// Should decrypt old data
	got, err := newRing.Decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt old ciphertext with rotated keyring: %v", err)
	}
	if string(got) != "old-secret" {
		t.Errorf("got %q, want %q", got, "old-secret")
	}

	// New encryption uses v2
	ct2, _ := newRing.Encrypt([]byte("new-secret"))
	got2, _ := newRing.Decrypt(ct2)
	if string(got2) != "new-secret" {
		t.Errorf("got %q, want %q", got2, "new-secret")
	}

	// Verify primary version is 2
	if newRing.PrimaryVersion() != 2 {
		t.Errorf("primary version = %d, want 2", newRing.PrimaryVersion())
	}
}

func TestKeyRingLegacyFallback(t *testing.T) {
	key := randomHexKey()

	// Encrypt without keyring (legacy format: no version header)
	enc, _ := NewEncryptor(key)
	ct, _ := enc.Encrypt([]byte("legacy-data"))

	// KeyRing should still decrypt legacy data via fallback
	ring, _ := NewKeyRing(map[uint16]string{1: key})
	got, err := ring.Decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt legacy ciphertext: %v", err)
	}
	if string(got) != "legacy-data" {
		t.Errorf("got %q, want %q", got, "legacy-data")
	}
}

func TestKeyRingAsEncryptor(t *testing.T) {
	key := randomHexKey()
	ring, _ := NewKeyRing(map[uint16]string{1: key})

	// AsEncryptor returns an Encryptor that delegates to the KeyRing
	enc := ring.AsEncryptor()
	ct, err := enc.Encrypt([]byte("via-encryptor"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := enc.Decrypt(ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "via-encryptor" {
		t.Errorf("got %q, want %q", got, "via-encryptor")
	}
}
