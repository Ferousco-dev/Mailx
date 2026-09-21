// Package secretbox is MailX's one authenticated-encryption primitive for
// secrets stored at rest (AES-256-GCM with a fresh random nonce per message and
// caller-supplied associated data). Webhook signing secrets and DKIM private
// keys both use it, each under its OWN master key so the two purposes never
// share key material.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Box seals and opens secrets under one 32-byte key.
type Box struct {
	aead cipher.AEAD
	rand io.Reader
}

// DecodeKey decodes a base64 32-byte master key from an environment value. The
// error names the variable but never the value.
func DecodeKey(encoded, envName string) ([]byte, error) {
	if encoded == "" {
		return nil, fmt.Errorf("%s is required", envName)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must be base64-encoded 32 bytes", envName)
	}
	return key, nil
}

// New builds a Box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("secretbox: AES-256 key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, rand: rand.Reader}, nil
}

// Encrypt seals plaintext with a fresh random nonce. associatedData binds the
// ciphertext to its context (for example the owning row): opening under
// different associated data fails.
func (b *Box) Encrypt(plaintext, associatedData []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(b.rand, nonce); err != nil {
		return nil, nil, errors.New("secretbox: cannot generate nonce")
	}
	return b.aead.Seal(nil, nonce, plaintext, associatedData), nonce, nil
}

// Decrypt opens a sealed secret. The error never includes ciphertext or key data.
func (b *Box) Decrypt(ciphertext, nonce, associatedData []byte) ([]byte, error) {
	if len(nonce) != b.aead.NonceSize() {
		return nil, errors.New("secretbox: invalid nonce")
	}
	plain, err := b.aead.Open(nil, nonce, ciphertext, associatedData)
	if err != nil {
		return nil, errors.New("secretbox: authentication failed")
	}
	return plain, nil
}
