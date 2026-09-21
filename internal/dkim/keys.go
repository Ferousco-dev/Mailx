// Package dkim signs outbound MailX messages (RFC 6376) and manages the
// lifecycle of the signing keys.
//
// Algorithm decision (RFC 8301): rsa-sha256 with 2048-bit keys. rsa-sha1 is
// forbidden (RFC 8301 3.1), signers must use at least 1024 bits and should use
// 2048 (3.2); 2048 bits fits a DNS TXT record (two character-strings), while
// larger keys risk oversized DNS answers. Ed25519 (RFC 8463) is deferred:
// receiver support is uneven, so it would require dual signing to be useful.
// All cryptography comes from crypto/rsa, crypto/sha256, crypto/rand and
// crypto/x509; this package only assembles the DKIM protocol around them.
package dkim

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
)

const (
	// Algorithm is the only signing algorithm MailX supports.
	Algorithm = "rsa-sha256"
	// KeyBits is the RSA modulus size for newly generated keys.
	KeyBits = 2048
	minBits = 2048
)

// GenerateKey creates a fresh RSA signing key from crypto/rand.
func GenerateKey() (*rsa.PrivateKey, error) {
	k, err := rsa.GenerateKey(rand.Reader, KeyBits)
	if err != nil {
		return nil, errors.New("dkim: key generation failed")
	}
	return k, nil
}

// MarshalPrivate encodes a key as PKCS#8 DER (the plaintext that gets encrypted
// at rest; it never leaves the process unencrypted).
func MarshalPrivate(k *rsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, errors.New("dkim: cannot encode private key")
	}
	return der, nil
}

// ParsePrivate decodes PKCS#8 DER and enforces the algorithm and size policy:
// only RSA keys of at least 2048 bits are accepted.
func ParsePrivate(der []byte) (*rsa.PrivateKey, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("dkim: stored private key is not valid PKCS#8")
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("dkim: stored private key is not RSA")
	}
	if k.N.BitLen() < minBits {
		return nil, fmt.Errorf("dkim: key is smaller than %d bits", minBits)
	}
	return k, nil
}

// PublicKeyBase64 returns the base64 SubjectPublicKeyInfo used in the DKIM
// record's p= tag. It contains no private material.
func PublicKeyBase64(k *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		return "", errors.New("dkim: cannot encode public key")
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// DNSName is the TXT record owner name: <selector>._domainkey.<domain>.
func DNSName(selector, domain string) string { return selector + "._domainkey." + domain }

// DNSValue is the TXT record value for a base64 public key.
func DNSValue(publicKeyB64 string) string { return "v=DKIM1; k=rsa; p=" + publicKeyB64 }

// DNSChunks splits a TXT value into DNS character-strings of at most 255
// bytes, for display to operators whose DNS provider needs them split.
func DNSChunks(value string) []string {
	var out []string
	for len(value) > 255 {
		out = append(out, value[:255])
		value = value[255:]
	}
	return append(out, value)
}
