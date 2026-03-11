// Package crypto provides AES-256-GCM encryption for secrets at rest.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
)

// Encryptor provides AES-256-GCM encryption/decryption using a 32-byte key.
// If keyRing is set, Encrypt/Decrypt delegate to it for versioned key support.
type Encryptor struct {
	gcm     cipher.AEAD
	keyRing *KeyRing // non-nil when created via KeyRing.AsEncryptor()
}

// NewEncryptor creates an Encryptor from a hex-encoded 32-byte key.
// The key should come from OPENSANDBOX_SECRET_ENCRYPTION_KEY.
func NewEncryptor(hexKey string) (*Encryptor, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decode hex key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes (64 hex chars), got %d bytes", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	return &Encryptor{gcm: gcm}, nil
}

// Encrypt returns nonce || ciphertext (suitable for storing as BYTEA).
// If a KeyRing is attached, delegates to it for versioned encryption.
func (e *Encryptor) Encrypt(plaintext []byte) ([]byte, error) {
	if e.keyRing != nil {
		return e.keyRing.Encrypt(plaintext)
	}
	nonce := make([]byte, e.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return e.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt expects nonce || ciphertext as produced by Encrypt.
// If a KeyRing is attached, delegates to it for versioned decryption.
func (e *Encryptor) Decrypt(data []byte) ([]byte, error) {
	if e.keyRing != nil {
		return e.keyRing.Decrypt(data)
	}
	nonceSize := e.gcm.NonceSize()
	if len(data) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := e.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}
