package idempotency

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantErr error
	}{
		{"empty", "", ErrEmptyKey},
		{"simple", "checkout-order-92831", nil},
		{"uuid", "7c1dbba5-3b9a-4e0e-9b0a-2b0a2b0a2b0a", nil},
		{"max length", strings.Repeat("a", MaxKeyLen), nil},
		{"too long", strings.Repeat("a", MaxKeyLen+1), ErrKeyTooLong},
		{"cr", "abc\rdef", ErrKeyControlChar},
		{"lf", "abc\ndef", ErrKeyControlChar},
		{"nul", "abc\x00def", ErrKeyControlChar},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateKey(c.key)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("ValidateKey(%q) = %v, want %v", c.key, err, c.wantErr)
			}
		})
	}
}
