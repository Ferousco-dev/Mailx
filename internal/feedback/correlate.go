package feedback

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrShortSecret means a Correlator secret was below the minimum entropy this
// package accepts.
var ErrShortSecret = errors.New("feedback: correlator secret must be at least 32 bytes")

// macLen is the truncated MAC length embedded in a token: 16 bytes = 128 bits,
// the same margin as MailX's own message IDs (internal/storage.NewID uses 16
// random bytes) — brute-forcing it is infeasible, and truncation keeps the
// token short without weakening it against forgery (HMAC truncation resistance
// does not depend on the untruncated length once >= 128 bits remain).
const macLen = 16

// Correlator issues and verifies opaque tokens that bind a MailX message ID to
// an HMAC, so a party that does not know the server secret cannot claim
// "this feedback is for message X" merely by knowing or guessing X.
//
// Design (docs/design-v0.32.md "Correlation"): message IDs are already 128-bit
// crypto-random (internal/storage.NewID) and never sequential, so they are not
// guessable — but guessability is only ONE of the two properties correlation
// needs. The other is: an attacker who can submit SOMETHING to the ingestion
// boundary must not be able to assert an arbitrary message ID as authoritative.
// Correlator.Verify makes that assertion cryptographically bound to a secret
// the ingestion boundary owns, independent of transport trust. The ingestion
// boundary (internal/api) ALSO requires its own operator credential for every
// request; the token is defense in depth for the (documented, deferred) case
// where feedback reaches MailX through a channel that isn't fully trusted
// end-to-end — see docs/design-v0.32.md "Return-Path" for why MailX does not
// yet also embed this token via envelope Return-Path (VERP).
type Correlator struct {
	secret []byte
}

// NewCorrelator returns a Correlator keyed by secret, which must be at least
// 32 bytes (matches MailX's other HMAC/AEAD key-length conventions, e.g.
// internal/secretbox).
func NewCorrelator(secret []byte) (*Correlator, error) {
	if len(secret) < 32 {
		return nil, ErrShortSecret
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &Correlator{secret: cp}, nil
}

func (c *Correlator) mac(messageID string) []byte {
	h := hmac.New(sha256.New, c.secret)
	h.Write([]byte("mailx-feedback-correlation-v1|"))
	h.Write([]byte(messageID))
	return h.Sum(nil)[:macLen]
}

// Token returns an opaque correlation token for messageID: "<messageID>.<mac>",
// hex-encoded. It reveals nothing beyond the message ID itself (which is
// already opaque and carries no tenant information) plus an unforgeable MAC.
func (c *Correlator) Token(messageID string) (string, error) {
	if messageID == "" {
		return "", errors.New("feedback: message ID is empty")
	}
	return messageID + "." + hex.EncodeToString(c.mac(messageID)), nil
}

// Verify parses and checks token, returning the message ID it names only when
// the MAC is valid. Comparison is constant-time (hmac.Equal). An invalid,
// tampered, truncated, or garbage token returns ok=false — the caller must
// never proceed to look up or mutate state on a false result.
func (c *Correlator) Verify(token string) (messageID string, ok bool) {
	i := strings.LastIndexByte(token, '.')
	if i <= 0 || i == len(token)-1 {
		return "", false
	}
	id, macHex := token[:i], token[i+1:]
	got, err := hex.DecodeString(macHex)
	if err != nil || len(got) != macLen {
		return "", false
	}
	want := c.mac(id)
	if !hmac.Equal(got, want) {
		return "", false
	}
	return id, true
}
