package humanauth

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 default algorithm; HMAC-SHA1 is not broken as a MAC.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"time"
)

// Hand-rolled RFC 4226 (HOTP) / RFC 6238 (TOTP) — see DEC-229. Parameters are
// the authenticator-app defaults: HMAC-SHA1, 30-second step, 6 digits, and a
// ±1 step verification window for clock skew.
const (
	totpPeriod = 30
	totpDigits = 6
	totpSkew   = 1
)

var totpB32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// hotp computes RFC 4226 section 5.3: HMAC-SHA1 over the 8-byte big-endian
// counter, dynamic truncation, modulo 10^digits.
func hotp(key []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, code%mod)
}

func totpStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// verifyTOTP returns the matched step for code within ±totpSkew of now. Every
// candidate is compared in constant time and all candidates are checked.
func verifyTOTP(key []byte, code string, now time.Time) (int64, bool) {
	if len(code) != totpDigits {
		return 0, false
	}
	cur := totpStep(now)
	var matched int64
	ok := false
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		s := cur + d
		if s < 0 {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(s), totpDigits)), []byte(code)) == 1 && !ok {
			matched, ok = s, true
		}
	}
	return matched, ok
}

// otpauthURI builds the Key URI Format understood by authenticator apps.
func otpauthURI(issuer, account string, key []byte) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", totpB32.EncodeToString(key))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
