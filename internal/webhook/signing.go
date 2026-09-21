package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

const SignatureTolerance = 5 * time.Minute

func Signature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature is safe example code for consumers and is also used by
// tests. It enforces freshness and compares MAC bytes in constant time.
func VerifySignature(secret, timestamp, signature string, body []byte, now time.Time, tolerance time.Duration) error {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || tolerance <= 0 {
		return errors.New("webhook: malformed timestamp")
	}
	signedAt := time.Unix(seconds, 0)
	if signedAt.Before(now.Add(-tolerance)) || signedAt.After(now.Add(tolerance)) {
		return errors.New("webhook: stale timestamp")
	}
	if !strings.HasPrefix(signature, "v1=") {
		return errors.New("webhook: malformed signature")
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, "v1="))
	if err != nil {
		return errors.New("webhook: malformed signature")
	}
	expectedText := Signature(secret, timestamp, body)
	expected, _ := hex.DecodeString(strings.TrimPrefix(expectedText, "v1="))
	if !hmac.Equal(provided, expected) {
		return errors.New("webhook: signature mismatch")
	}
	return nil
}
