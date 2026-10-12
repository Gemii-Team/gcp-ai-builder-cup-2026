// Package vault encrypts the Google refresh tokens stored in identity.oauth_tokens
// with AES-256-GCM, so a database dump alone does not hand out access to users'
// Google accounts.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

const keyBytes = 32

// Vault seals and opens values under one key. It is safe for concurrent use.
type Vault struct {
	aead cipher.AEAD
}

// New returns a Vault for a key given as standard base64 of exactly 32 bytes (the
// form of KIWI_TOKEN_ENC_KEY). The error never contains the key.
func New(base64Key string) (*Vault, error) {
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil || len(key) != keyBytes {
		return nil, fmt.Errorf("vault: the key must be standard base64 of %d bytes (for example `openssl rand -base64 %d`)", keyBytes, keyBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault: gcm: %w", err)
	}
	return &Vault{aead: aead}, nil
}

// Encrypt seals plain under a fresh random nonce, which prefixes the result.
func (v *Vault) Encrypt(plain []byte) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize(), v.aead.NonceSize()+len(plain)+v.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("vault: nonce: %w", err)
	}
	return v.aead.Seal(nonce, nonce, plain, nil), nil
}

// Decrypt opens a value sealed by Encrypt under the same key. A value that was
// altered, truncated or sealed under another key is an error.
func (v *Vault) Decrypt(ct []byte) ([]byte, error) {
	n := v.aead.NonceSize()
	if len(ct) < n+v.aead.Overhead() {
		return nil, errors.New("vault: ciphertext is too short")
	}
	plain, err := v.aead.Open(nil, ct[:n], ct[n:], nil)
	if err != nil {
		return nil, errors.New("vault: ciphertext is not authentic")
	}
	return plain, nil
}
