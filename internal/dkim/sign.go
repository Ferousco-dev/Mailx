package dkim

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

// Sign errors are bounded categories; none carries key or message data.
var (
	ErrMalformedMessage = errors.New("dkim: message has no CRLF header/body separator or a malformed header")
	ErrNoFrom           = errors.New("dkim: message has no single parseable From header")
	ErrDomainMismatch   = errors.New("dkim: From domain does not match the signing domain")
	ErrSignFailed       = errors.New("dkim: signing failed")
)

// signedHeaders is the ordered list of header names MailX signs when present.
// From is mandatory (RFC 6376 5.4). Trace headers (Received, Return-Path) are
// never signed. Bcc never exists in built messages, so it cannot be signed.
var signedHeaders = []string{"from", "to", "cc", "subject", "date", "message-id",
	"mime-version", "content-type", "content-transfer-encoding", "reply-to"}

// Options controls one signature. Domain is the d= value and MUST equal the
// message's From domain (it is the authorized domain, already lowercased).
type Options struct {
	Domain   string
	Selector string
	Key      *rsa.PrivateKey
	Now      time.Time
}

type headerField struct {
	name string // as written
	raw  string // name, colon, value, folds included, no trailing CRLF
}

// Sign returns raw with a DKIM-Signature header prepended, using
// relaxed/relaxed canonicalization and rsa-sha256. The returned bytes are the
// only representation that may be stored and transmitted: any later change to a
// signed header or the body invalidates the signature.
func Sign(raw []byte, o Options) ([]byte, error) {
	if o.Key == nil || o.Domain == "" || o.Selector == "" {
		return nil, ErrSignFailed
	}
	headerBlock, body, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		return nil, ErrMalformedMessage
	}
	fields, err := parseHeaders(string(headerBlock))
	if err != nil {
		return nil, err
	}
	if err := checkFrom(fields, o.Domain); err != nil {
		return nil, err
	}

	names, chosen := selectHeaders(fields)
	bodySum := sha256.Sum256(relaxedBody(body))
	bh := base64.StdEncoding.EncodeToString(bodySum[:])

	unsigned := buildSignatureHeader(o, names, bh)
	h := sha256.New()
	for _, f := range chosen {
		h.Write([]byte(relaxedHeader(f.raw) + "\r\n"))
	}
	h.Write([]byte(relaxedHeader(unsigned))) // b= empty, no trailing CRLF (RFC 6376 3.7)
	sig, err := rsa.SignPKCS1v15(rand.Reader, o.Key, crypto.SHA256, h.Sum(nil))
	if err != nil {
		return nil, ErrSignFailed
	}
	header := unsigned + foldBase64(base64.StdEncoding.EncodeToString(sig)) + "\r\n"
	return append([]byte(header), raw...), nil
}

func parseHeaders(block string) ([]headerField, error) {
	var fields []headerField
	for _, line := range strings.Split(block, "\r\n") {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' { // continuation
			if len(fields) == 0 {
				return nil, ErrMalformedMessage
			}
			fields[len(fields)-1].raw += "\r\n" + line
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, ErrMalformedMessage
		}
		fields = append(fields, headerField{name: name, raw: line})
	}
	return fields, nil
}

// checkFrom requires exactly one From header whose parsed domain equals the
// signing domain, so a key can never sign for another domain's message.
func checkFrom(fields []headerField, domain string) error {
	var from []headerField
	for _, f := range fields {
		if strings.EqualFold(f.name, "from") {
			from = append(from, f)
		}
	}
	if len(from) != 1 {
		return ErrNoFrom
	}
	_, value, _ := strings.Cut(from[0].raw, ":")
	addr, err := mail.ParseAddress(strings.ReplaceAll(value, "\r\n", ""))
	if err != nil {
		return ErrNoFrom
	}
	at := strings.LastIndexByte(addr.Address, '@')
	if at < 0 || !strings.EqualFold(strings.TrimSuffix(addr.Address[at+1:], "."), domain) {
		return ErrDomainMismatch
	}
	return nil
}

// selectHeaders returns the h= names and the header instances to hash, in h=
// order. Repeated names are signed bottom-up (RFC 6376 5.4.2).
func selectHeaders(fields []headerField) (names []string, chosen []headerField) {
	for _, want := range signedHeaders {
		for i := len(fields) - 1; i >= 0; i-- {
			if strings.EqualFold(fields[i].name, want) {
				names = append(names, want)
				chosen = append(chosen, fields[i])
			}
		}
	}
	return names, chosen
}

// buildSignatureHeader renders the DKIM-Signature field up to and including
// "b=" (value empty), folded at tag boundaries. The same text is hashed and
// then extended with the folded signature, so canonicalization sees identical
// whitespace on both sides.
func buildSignatureHeader(o Options, names []string, bh string) string {
	// Segments end at a legal fold point: after a tag's ';' or after a ':' in h=.
	var segs []string
	segs = append(segs, "DKIM-Signature: v=1;", " a="+Algorithm+";", " c=relaxed/relaxed;", " d="+o.Domain+";",
		" s="+o.Selector+";", " t="+strconv.FormatInt(o.Now.Unix(), 10)+";")
	for i, n := range names {
		seg := n
		switch {
		case i == 0:
			seg = " h=" + n
		}
		if i < len(names)-1 {
			seg += ":"
		} else {
			seg += ";"
		}
		segs = append(segs, seg)
	}
	segs = append(segs, " bh="+bh+";", " b=")

	var out strings.Builder
	line := 0
	for i, seg := range segs {
		if i > 0 && (line+len(seg) > 76 || i == len(segs)-1) { // b= always starts a fresh line
			out.WriteString("\r\n\t")
			seg = strings.TrimPrefix(seg, " ")
			line = 1
		}
		out.WriteString(seg)
		line += len(seg)
	}
	return out.String()
}

// foldBase64 splits the signature into 64-character segments joined by folds.
func foldBase64(s string) string {
	var b strings.Builder
	for len(s) > 64 {
		b.WriteString(s[:64] + "\r\n\t")
		s = s[64:]
	}
	b.WriteString(s)
	return b.String()
}

// String helpers for tests and diagnostics never include key material.
func (o Options) String() string { return fmt.Sprintf("dkim.Options{%s/%s}", o.Selector, o.Domain) }
