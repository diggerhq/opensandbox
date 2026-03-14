package crypto

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func TestEncryptDecrypt(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	hexKey := hex.EncodeToString(key)

	enc, err := NewEncryptor(hexKey)
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("sk-ant-api03-super-secret-key-1234567890")
	ciphertext, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}

	if string(ciphertext) == string(plaintext) {
		t.Error("ciphertext should not equal plaintext")
	}

	decrypted, err := enc.Decrypt(ciphertext)
	if err != nil {
		t.Fatal(err)
	}

	if string(decrypted) != string(plaintext) {
		t.Errorf("got %q, want %q", decrypted, plaintext)
	}
}

func TestEncryptDifferentNonces(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	enc, _ := NewEncryptor(hex.EncodeToString(key))

	plaintext := []byte("same-value")
	ct1, _ := enc.Encrypt(plaintext)
	ct2, _ := enc.Encrypt(plaintext)

	if string(ct1) == string(ct2) {
		t.Error("two encryptions of the same value should produce different ciphertexts (unique nonce)")
	}
}

func TestBadKey(t *testing.T) {
	_, err := NewEncryptor("tooshort")
	if err == nil {
		t.Error("expected error for short key")
	}

	_, err = NewEncryptor("not-hex-at-all!")
	if err == nil {
		t.Error("expected error for non-hex key")
	}
}

func TestDecryptTampered(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	enc, _ := NewEncryptor(hex.EncodeToString(key))

	ct, _ := enc.Encrypt([]byte("secret"))
	ct[len(ct)-1] ^= 0xFF // flip last byte

	_, err := enc.Decrypt(ct)
	if err == nil {
		t.Error("expected error decrypting tampered ciphertext")
	}
}
