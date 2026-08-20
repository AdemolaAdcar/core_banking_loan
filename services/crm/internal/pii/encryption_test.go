package pii

import (
	"bytes"
	"strings"
	"testing"
)

func testKey() []byte {
	return bytes.Repeat([]byte{0x42}, 32)
}

func TestAESGCMEncryptor_RoundTrip(t *testing.T) {
	enc, err := NewAESGCMEncryptor(testKey())
	if err != nil {
		t.Fatalf("NewAESGCMEncryptor: %v", err)
	}

	plaintext := "123-45-6789"
	ciphertext, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if ciphertext == plaintext {
		t.Fatal("ciphertext must not equal plaintext")
	}
	if strings.Contains(ciphertext, plaintext) {
		t.Fatal("ciphertext must not contain the plaintext SSN in any recognizable form")
	}

	got, err := enc.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plaintext {
		t.Errorf("Decrypt(Encrypt(x)) = %q, want %q", got, plaintext)
	}
}

func TestAESGCMEncryptor_EmptyStringPassesThrough(t *testing.T) {
	enc, _ := NewAESGCMEncryptor(testKey())
	ciphertext, err := enc.Encrypt("")
	if err != nil || ciphertext != "" {
		t.Errorf("Encrypt(\"\") = (%q, %v), want (\"\", nil)", ciphertext, err)
	}
	plaintext, err := enc.Decrypt("")
	if err != nil || plaintext != "" {
		t.Errorf("Decrypt(\"\") = (%q, %v), want (\"\", nil)", plaintext, err)
	}
}

func TestAESGCMEncryptor_TamperedCiphertextFailsClosed(t *testing.T) {
	enc, _ := NewAESGCMEncryptor(testKey())
	ciphertext, _ := enc.Encrypt("sensitive-value")

	tampered := []byte(ciphertext)
	tampered[len(tampered)-1] ^= 0xFF // flip bits in the last byte
	_, err := enc.Decrypt(string(tampered))
	if err == nil {
		t.Fatal("expected Decrypt to fail closed on tampered ciphertext, got nil error")
	}
}

func TestAESGCMEncryptor_TwoEncryptionsOfSameValueDifferByNonce(t *testing.T) {
	// GCM must use a fresh random nonce every call -- two ciphertexts of
	// the same plaintext must never be identical (that would leak that
	// two records share a value, e.g. two applicants with the same SSN,
	// directly from the ciphertext without ever decrypting anything).
	enc, _ := NewAESGCMEncryptor(testKey())
	a, _ := enc.Encrypt("123-45-6789")
	b, _ := enc.Encrypt("123-45-6789")
	if a == b {
		t.Fatal("two encryptions of the same plaintext must not produce identical ciphertext")
	}
}

func TestNewAESGCMEncryptor_RejectsWrongKeyLength(t *testing.T) {
	_, err := NewAESGCMEncryptor([]byte("too-short"))
	if err == nil {
		t.Fatal("expected an error for a non-32-byte key, got nil")
	}
}

func TestAESGCMEncryptor_WrongKeyCannotDecrypt(t *testing.T) {
	encA, _ := NewAESGCMEncryptor(testKey())
	otherKey := bytes.Repeat([]byte{0x99}, 32)
	encB, _ := NewAESGCMEncryptor(otherKey)

	ciphertext, _ := encA.Encrypt("secret")
	if _, err := encB.Decrypt(ciphertext); err == nil {
		t.Fatal("decrypting with the wrong key must fail, not silently succeed")
	}
}
