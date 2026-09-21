package webhook

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"github.com/Ferousco-dev/mailx/internal/secretbox"
)

const secretBytes = 32

// SecretBox encrypts webhook signing secrets. It is a string-typed wrapper over
// the shared internal/secretbox primitive.
type SecretBox struct{ box *secretbox.Box }

func DecodeMasterKey(encoded string) ([]byte, error) {
	key, err := secretbox.DecodeKey(encoded, "MAILX_WEBHOOK_MASTER_KEY")
	if err != nil {
		return nil, fmt.Errorf("webhook: %w", err)
	}
	return key, nil
}

func NewSecretBox(key []byte) (*SecretBox, error) {
	box, err := secretbox.New(key)
	if err != nil {
		return nil, errors.New("webhook: AES-256 key must be 32 bytes")
	}
	return &SecretBox{box: box}, nil
}

func GenerateSecret() (string, error) {
	raw := make([]byte, secretBytes)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("webhook: generate signing secret: %w", err)
	}
	return "whsec_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func (b *SecretBox) Encrypt(plaintext string, associatedData []byte) (ciphertext, nonce []byte, err error) {
	ciphertext, nonce, err = b.box.Encrypt([]byte(plaintext), associatedData)
	if err != nil {
		return nil, nil, fmt.Errorf("webhook: generate encryption nonce: %w", err)
	}
	return ciphertext, nonce, nil
}

func (b *SecretBox) Decrypt(ciphertext, nonce, associatedData []byte) (string, error) {
	plain, err := b.box.Decrypt(ciphertext, nonce, associatedData)
	if err != nil {
		return "", errors.New("webhook: encrypted secret authentication failed")
	}
	return string(plain), nil
}
