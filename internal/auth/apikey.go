// Package auth implements MailX's application/developer API-key
// authentication: format, generation, and secret verification. It knows
// nothing about HTTP — see internal/api/authmiddleware.go for the request
// boundary, and Service in service.go for the PostgreSQL-backed lifecycle
// (create/authenticate/rotate/revoke).
//
// Threat model (verifier design): a MailX API key is a 256-bit
// crypto/rand secret, not a human password - it cannot be guessed or
// brute-forced regardless of how fast it is checked, so a slow
// password-hashing scheme (bcrypt/scrypt/argon2) buys nothing here and
// would only add CPU cost to every single authenticated request. What
// PostgreSQL stores is HMAC-SHA256(pepper, secret): with a pepper
// configured, a PostgreSQL-only leak (dump/backup, no server config)
// gives an attacker verifiers they cannot use to authenticate, because
// computing the same HMAC requires the pepper, which never leaves
// process memory or configuration. Without a pepper (a valid, documented
// local-dev choice - see Service's doc), the stored value degrades to a
// plain SHA-256 of the secret: still safe against brute-forcing the
// 256-bit secret itself, but a DB-only leak plus a stolen server config
// becomes irrelevant in that case, since there is no separate secret
// protecting the hash - PostgreSQL access alone reveals nothing usable
// either way, because in both cases the ONLY thing stored is a
// preimage-resistant hash of a secret that cannot practically be
// recovered from that hash. The pepper's actual value is narrower: it
// means a DB-only leak cannot even be used to verify a GUESSED key
// (irrelevant against random secrets) and, more importantly, it lets
// MailX invalidate every stored verifier at once by rotating the pepper
// if it is ever suspected the hashes themselves leaked in a form an
// attacker could target - see Service's doc for that operational detail.
// What NEITHER protects against: a leaked RAW key (e.g. from a client's
// own logs) authenticates exactly as before until revoked - hashing the
// server's copy cannot detect or prevent misuse of a copy the attacker
// already has in the clear.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	keyPrefix    = "mx"
	keyIDBytes   = 16 // 128 bits: a safe, non-secret lookup identifier, not a security boundary
	secretBytes  = 32 // 256 bits: the actual credential; brute-forcing this is infeasible
	keyIDHexLen  = keyIDBytes * 2
	secretHexLen = secretBytes * 2
	maxRawKeyLen = 8 + keyIDHexLen + secretHexLen // generous bound checked before any parsing work
)

var ErrMalformedKey = errors.New("auth: malformed api key")

// Scope is one of the small, fixed set of permissions v0.19 recognizes.
// Deliberately not a free-form string: an unrecognized scope must be
// rejected at creation time, never silently accepted.
type Scope string

const (
	ScopeEmailsSend Scope = "emails:send"
	ScopeEmailsRead Scope = "emails:read"
)

// ValidScopes lists every scope MailX currently understands - grown only
// when a real route needs a new permission, never speculatively.
var ValidScopes = []Scope{ScopeEmailsSend, ScopeEmailsRead}

func ValidScope(s string) bool {
	for _, v := range ValidScopes {
		if string(v) == s {
			return true
		}
	}
	return false
}

// Generated is a freshly minted credential: Raw is shown to the caller
// exactly once and never stored; KeyID/SecretHash are what persists.
type Generated struct {
	Raw        string
	KeyID      string
	SecretHash string
}

// Generate creates a new random key id + secret and returns the full
// "mx_<key_id>_<secret>" credential plus its durable (KeyID, SecretHash)
// pair. Both halves use crypto/rand — never math/rand, a timestamp, or a
// UUID — because both must be unpredictable: KeyID to prevent enumeration
// races/guessing which row an attacker is even targeting, and the secret
// because it is the actual credential.
func Generate(pepper []byte) (Generated, error) {
	keyID, err := randomHex(keyIDBytes)
	if err != nil {
		return Generated{}, fmt.Errorf("auth: generate key id: %w", err)
	}
	secret, err := randomHex(secretBytes)
	if err != nil {
		return Generated{}, fmt.Errorf("auth: generate secret: %w", err)
	}
	return Generated{
		Raw:        keyPrefix + "_" + keyID + "_" + secret,
		KeyID:      keyID,
		SecretHash: Hash(secret, pepper),
	}, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Parse splits a raw credential into its (KeyID, secret) halves without
// touching the database. It never panics on arbitrary input — every
// return path is a length/prefix/charset check before any indexing.
func Parse(raw string) (keyID, secret string, err error) {
	if len(raw) == 0 || len(raw) > maxRawKeyLen {
		return "", "", ErrMalformedKey
	}
	parts := strings.Split(raw, "_")
	if len(parts) != 3 || parts[0] != keyPrefix {
		return "", "", ErrMalformedKey
	}
	keyID, secret = parts[1], parts[2]
	if len(keyID) != keyIDHexLen || len(secret) != secretHexLen {
		return "", "", ErrMalformedKey
	}
	if !isHex(keyID) || !isHex(secret) {
		return "", "", ErrMalformedKey
	}
	return keyID, secret, nil
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// Hash computes the verifier stored in PostgreSQL. With a non-empty
// pepper this is HMAC-SHA256(pepper, secret); with none, plain
// SHA-256(secret) — see the package doc for exactly what that
// distinction does and does not protect against.
func Hash(secret string, pepper []byte) string {
	if len(pepper) == 0 {
		sum := sha256.Sum256([]byte(secret))
		return hex.EncodeToString(sum[:])
	}
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(secret))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify recomputes the hash and compares in constant time, so response
// timing cannot help an attacker distinguish "wrong secret" from
// "right secret, wrong something else" one byte at a time.
func Verify(secret, storedHash string, pepper []byte) bool {
	computed := Hash(secret, pepper)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(storedHash)) == 1
}
