package webhook

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

const secretBytes = 32

type SecretBox struct {
	aead cipher.AEAD
	rand io.Reader
}

func DecodeMasterKey(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, errors.New("webhook: MAILX_WEBHOOK_MASTER_KEY is required")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(key) != 32 {
		return nil, errors.New("webhook: master key must be base64-encoded 32 bytes")
	}
	return key, nil
}

func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != 32 {
		return nil, errors.New("webhook: AES-256 key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretBox{aead: aead, rand: rand.Reader}, nil
}

func GenerateSecret() (string, error) {
	raw := make([]byte, secretBytes)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("webhook: generate signing secret: %w", err)
	}
	return "whsec_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func (b *SecretBox) Encrypt(plaintext string, associatedData []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(b.rand, nonce); err != nil {
		return nil, nil, fmt.Errorf("webhook: generate encryption nonce: %w", err)
	}
	return b.aead.Seal(nil, nonce, []byte(plaintext), associatedData), nonce, nil
}

func (b *SecretBox) Decrypt(ciphertext, nonce, associatedData []byte) (string, error) {
	if len(nonce) != b.aead.NonceSize() {
		return "", errors.New("webhook: invalid encrypted secret nonce")
	}
	plain, err := b.aead.Open(nil, nonce, ciphertext, associatedData)
	if err != nil {
		return "", errors.New("webhook: encrypted secret authentication failed")
	}
	return string(plain), nil
}
