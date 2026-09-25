// Package humanauth implements MailX's human/browser authentication:
// signup, login, JWT access tokens, rotating refresh tokens, and
// organization (tenant) membership for human accounts. It is a
// deliberately separate code path from internal/auth (API-key
// authentication for machine/service callers) — the two authenticate
// different kinds of caller and must never be conflated. See
// .ilana/decisions.md DEC-205/DEC-206.
package humanauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrInvalidToken covers every way a JWT can fail verification
// (malformed, bad signature, wrong alg, expired) — callers never learn
// which, matching the anti-enumeration posture used elsewhere in this
// package.
var ErrInvalidToken = errors.New("humanauth: invalid or expired token")

// Claims is the minimal payload a MailX access token carries.
type Claims struct {
	HumanID string `json:"sub"`
	Role    string `json:"role"`
	IatUnix int64  `json:"iat"`
	ExpUnix int64  `json:"exp"`
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func b64Decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// signJWT hand-rolls a minimal HS256 JWT (header.payload.signature) using
// only the standard library — this repo keeps third-party dependencies
// to a minimum (see go.mod), and a hand-rolled HS256 token is a small,
// auditable amount of code for what's needed here.
func signJWT(secret []byte, claims Claims) (string, error) {
	header, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := b64(header) + "." + b64(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(unsigned))
	sig := mac.Sum(nil)
	return unsigned + "." + b64(sig), nil
}

// verifyJWT checks signature and expiry, returning the decoded claims.
func verifyJWT(secret []byte, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalidToken
	}
	unsigned := parts[0] + "." + parts[1]
	wantSig, err := b64Decode(parts[2])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(unsigned))
	gotSig := mac.Sum(nil)
	if !hmac.Equal(gotSig, wantSig) {
		return Claims{}, ErrInvalidToken
	}

	headerBytes, err := b64Decode(parts[0])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if header.Alg != "HS256" {
		return Claims{}, ErrInvalidToken
	}

	payloadBytes, err := b64Decode(parts[1])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var claims Claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if time.Now().Unix() >= claims.ExpUnix {
		return Claims{}, ErrInvalidToken
	}
	return claims, nil
}

// constantTimeEqual is used nowhere sensitive beyond what hmac.Equal /
// subtle.ConstantTimeCompare already give us; kept as a small helper for
// any future raw-token comparisons.
func constantTimeEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
