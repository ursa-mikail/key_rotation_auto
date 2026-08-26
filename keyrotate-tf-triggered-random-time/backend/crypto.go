package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"time"
)

// testPlaintext is the fixed value every key's liveness test vector
// encrypts. Its content doesn't matter; what matters is that every
// active key can produce it back from its own stored ciphertext.
var testPlaintext = []byte("key-rotation-liveness-check-v1")

// hkdfSHA256 is a small, dependency-free implementation of RFC 5869
// HKDF (Extract-and-Expand) using SHA-256. Kept in-house rather than
// pulling in golang.org/x/crypto so the whole backend has exactly one
// external dependency (the Postgres driver).
func hkdfSHA256(secret, salt, info []byte, length int) []byte {
	extractor := hmac.New(sha256.New, salt)
	extractor.Write(secret)
	prk := extractor.Sum(nil)

	okm := make([]byte, 0, length)
	var t []byte
	for counter := byte(1); len(okm) < length; counter++ {
		mac := hmac.New(sha256.New, prk)
		mac.Write(t)
		mac.Write(info)
		mac.Write([]byte{counter})
		t = mac.Sum(nil)
		okm = append(okm, t...)
	}
	return okm[:length]
}

// deriveNextKey implements "rotate a key by adding to it" as an HKDF
// step over the previous key material, salted with the rotation
// timestamp. This is a proper KDF chain, not literal concatenation:
// concatenating raw key bytes across generations would leak structure
// and never increases effective entropy. HKDF gives each generation a
// fresh, independent-looking 256-bit key that is still deterministically
// derived from its parent -- which is what "rotate by adding" should
// mean cryptographically.
func deriveNextKey(prevMaterial []byte, rotatedAt time.Time) ([]byte, error) {
	salt := []byte(rotatedAt.UTC().Format(time.RFC3339Nano))
	info := []byte("keyrotate-hkdf-v1")
	return hkdfSHA256(prevMaterial, salt, info, 32), nil // AES-256
}

// sealTestVector encrypts the fixed test plaintext under key, returning
// (ciphertext, nonce). Called once when a key is created.
func sealTestVector(key []byte) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("new gcm: %w", err)
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("read nonce: %w", err)
	}
	ciphertext = gcm.Seal(nil, nonce, testPlaintext, nil)
	return ciphertext, nonce, nil
}

// openTestVector decrypts ciphertext with key and confirms it matches
// testPlaintext exactly. This is the "live test" gate: every rotation
// tick, every non-retired key must pass this before any key gets
// promoted to primary.
func openTestVector(key, ciphertext, nonce []byte) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("new gcm: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return fmt.Errorf("decrypt failed: %w", err)
	}
	if string(plaintext) != string(testPlaintext) {
		return fmt.Errorf("decrypted plaintext mismatch")
	}
	return nil
}
