// Package pii is the one deliberate boundary in this service where
// plaintext PII is handled at rest — and only to encrypt it. Every field
// marked "x-pii: true" in the PartyAPI spec passes through Encryptor
// before it is ever written to the database. Nothing in this package
// logs its input or output; callers must not log plaintext PII before it
// reaches this package either.
package pii

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Encryptor is the field-level encryption boundary every PII field must
// pass through before persistence, and after retrieval before it is
// handed back to a request handler. Deliberately a narrow interface — a
// KMS-backed implementation, an HSM-backed implementation, or (as here)
// a local AES-GCM implementation can all satisfy it, so the choice of
// key-management backend is swappable without touching any caller.
type Encryptor interface {
	// Encrypt returns an opaque, base64-encoded ciphertext string safe to
	// store in a database column. The empty string encrypts to the empty
	// string (so an optional PII field like Email can stay optional
	// without every caller special-casing "no value").
	Encrypt(plaintext string) (string, error)

	// Decrypt reverses Encrypt. Returns an error, never a partial or
	// best-effort plaintext, if ciphertext has been tampered with —
	// AES-GCM is authenticated, so tampering is always detected, never
	// silently decrypted into garbage.
	Decrypt(ciphertext string) (string, error)
}

// AESGCMEncryptor is a local, single-key AES-256-GCM implementation of
// Encryptor. In production this key is expected to come from a KMS
// envelope-decryption call at process startup, never a hardcoded value —
// NewAESGCMEncryptor takes the already-resolved 32-byte key, it does not
// resolve one itself.
type AESGCMEncryptor struct {
	aead cipher.AEAD
}

// NewAESGCMEncryptor builds an Encryptor from a 32-byte (AES-256) key.
// Returns an error for any other key length rather than silently
// truncating or padding it.
func NewAESGCMEncryptor(key []byte) (*AESGCMEncryptor, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("pii: key must be 32 bytes for AES-256, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("pii: creating AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("pii: creating GCM mode: %w", err)
	}
	return &AESGCMEncryptor{aead: aead}, nil
}

func (e *AESGCMEncryptor) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("pii: generating nonce: %w", err)
	}
	ciphertext := e.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (e *AESGCMEncryptor) Decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("pii: decoding ciphertext: %w", err)
	}
	nonceSize := e.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("pii: ciphertext too short to contain a nonce")
	}
	nonce, sealed := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := e.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		// Deliberately do not include the underlying err in a form that
		// could leak timing/oracle information beyond "it failed" — GCM's
		// own Open already returns a generic error, this just documents
		// the intent.
		return "", errors.New("pii: decryption failed (ciphertext tampered with or wrong key)")
	}
	return string(plaintext), nil
}
