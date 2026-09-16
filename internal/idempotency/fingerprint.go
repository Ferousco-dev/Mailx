package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Fingerprint hashes the canonical JSON encoding of v, which must be a Go
// struct (never a map) — encoding/json always serializes a struct's
// fields in the struct's declared order, so two calls with equal field
// values always produce byte-identical JSON regardless of how the
// original HTTP request's JSON happened to order or whitespace its
// fields. This deliberately fingerprints the ALREADY-VALIDATED,
// normalized request value the handler built, not the raw request body —
// so "same request, different key order/whitespace" always matches, and
// callers control exactly which fields participate by choosing v's type.
func Fingerprint(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("idempotency: fingerprint: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
