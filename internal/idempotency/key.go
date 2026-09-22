// Package idempotency provides the pure, DB-independent pieces of v0.20's
// HTTP idempotency support: validating a client-supplied Idempotency-Key
// and computing a stable fingerprint of a validated request. Claiming,
// polling, and replay orchestration live in internal/api/email_handler.go
// — this package has exactly one caller today, so it stays two small
// files rather than growing a speculative service/orchestration layer.
package idempotency

import (
	"errors"
	"strings"
)

// MaxKeyLen matches the common convention (Stripe, and others) of
// bounding an idempotency key to 255 bytes — enough for a UUID, an order
// ID, or any reasonable client-generated token, small enough to keep the
// column, its index, and log lines bounded.
const MaxKeyLen = 255

// OperationEmailsCreate scopes idempotency for POST /v1/emails. Stable
// and independent of Go/HTTP internals (not a route path or function
// name) so refactors never accidentally change idempotency identity.
const OperationEmailsCreate = "emails.create"

// OperationBroadcastsCreate scopes idempotency for POST /v1/broadcasts.
const OperationBroadcastsCreate = "broadcasts.create"

var (
	ErrEmptyKey       = errors.New("idempotency: key is empty")
	ErrKeyTooLong     = errors.New("idempotency: key exceeds maximum length")
	ErrKeyControlChar = errors.New("idempotency: key contains a forbidden control character")
)

// ValidateKey rejects anything that must never reach PostgreSQL, logs, or
// an index: empty, oversized, or containing CR/LF/NUL. Otherwise the key
// is treated as a fully opaque client identifier — MailX never parses or
// assigns meaning to its contents.
func ValidateKey(raw string) error {
	if raw == "" {
		return ErrEmptyKey
	}
	if len(raw) > MaxKeyLen {
		return ErrKeyTooLong
	}
	if strings.ContainsAny(raw, "\r\n\x00") {
		return ErrKeyControlChar
	}
	return nil
}
