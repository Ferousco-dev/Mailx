package webhook

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestSecretGenerationAndEncryption(t *testing.T) {
	secret1, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	secret2, _ := GenerateSecret()
	if !strings.HasPrefix(secret1, "whsec_") || secret1 == secret2 || len(secret1) < 40 {
		t.Fatalf("unexpected generated secrets")
	}
	box, _ := NewSecretBox(bytes.Repeat([]byte{1}, 32))
	cipher1, nonce1, err := box.Encrypt(secret1, []byte("tenant-a"))
	if err != nil {
		t.Fatal(err)
	}
	cipher2, nonce2, _ := box.Encrypt(secret1, []byte("tenant-a"))
	if bytes.Equal(nonce1, nonce2) || bytes.Equal(cipher1, cipher2) {
		t.Fatal("AES-GCM nonce/ciphertext was reused")
	}
	plain, err := box.Decrypt(cipher1, nonce1, []byte("tenant-a"))
	if err != nil || plain != secret1 {
		t.Fatalf("round-trip failed: %q %v", plain, err)
	}
	cipher1[0] ^= 1
	if _, err := box.Decrypt(cipher1, nonce1, []byte("tenant-a")); err == nil {
		t.Fatal("corrupt ciphertext must fail closed")
	}
	other, _ := NewSecretBox(bytes.Repeat([]byte{2}, 32))
	if _, err := other.Decrypt(cipher2, nonce2, []byte("tenant-a")); err == nil {
		t.Fatal("wrong master key must fail closed")
	}
	if _, err := box.Decrypt(cipher2, nonce2, []byte("tenant-b")); err == nil {
		t.Fatal("ciphertext must be bound to its tenant")
	}
}

func TestMasterKeyValidation(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if key, err := DecodeMasterKey(encoded); err != nil || len(key) != 32 {
		t.Fatalf("valid key rejected: %v", err)
	}
	for _, bad := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := DecodeMasterKey(bad); err == nil {
			t.Fatalf("invalid key %q accepted", bad)
		}
	}
}
