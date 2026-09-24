package tracking

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

type Payload struct {
	TenantID  string `json:"t"`
	MessageID string `json:"m"`
	Recipient string `json:"r"`
	URL       string `json:"u,omitempty"`
}

var ErrInvalidToken = errors.New("tracking: invalid token")

func Sign(secret []byte, p Payload) (string, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(enc))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return enc + "." + sig, nil
}

func Verify(secret []byte, token string) (Payload, error) {
	enc, sig, ok := strings.Cut(token, ".")
	if !ok || enc == "" || sig == "" {
		return Payload{}, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(enc))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return Payload{}, ErrInvalidToken
	}
	body, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return Payload{}, ErrInvalidToken
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Payload{}, ErrInvalidToken
	}
	if p.TenantID == "" || p.MessageID == "" {
		return Payload{}, ErrInvalidToken
	}
	return p, nil
}
