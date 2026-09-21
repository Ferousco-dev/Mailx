package suppression

import (
	"errors"
	"net/mail"
	"strings"
)

// ErrInvalidAddress is returned for an address MailX cannot key.
var ErrInvalidAddress = errors.New("suppression: invalid email address")

const (
	maxAddressBytes = 254
	maxLocalBytes   = 64
	maxDomainBytes  = 253
)

// Normalize is THE canonical suppression key function. The API, the worker and
// the store all call it; nothing else derives a suppression key.
//
// Rules:
//   - one surrounding whitespace trim and one "<...>" envelope bracket pair are
//     removed (the worker sees bracket-form envelope recipients, callers send bare
//     addresses);
//   - the address must parse as a single RFC 5322 addr-spec with no display name;
//   - ASCII only. MailX supports no SMTPUTF8/IDN recipients anywhere, and a key
//     must not depend on Unicode normalization;
//   - the local part must be a plain dot-atom (no quoted forms) within 64 bytes;
//   - the domain is lower-cased, loses ONE trailing root dot, and must be valid
//     ASCII LDH labels (no IP literals);
//   - the LOCAL PART IS LOWER-CASED for the KEY ONLY. RFC 5321 2.4 requires SMTP
//     implementations to preserve local-part case in transport (MailX still sends
//     the address exactly as given), but also says exploiting local-part case
//     sensitivity "impedes interoperability and is discouraged". For a deny list
//     the safe direction is to treat case variants as the same recipient: the only
//     cost is a hypothetical mailbox that differs from another only by case, while
//     the alternative lets a suppressed recipient receive mail by changing case.
//   - NO provider-specific rules: dots and "+tag" are significant and never folded
//     (john.smith@gmail.com and johnsmith@gmail.com are different keys).
func Normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 && s[0] == '<' && s[len(s)-1] == '>' {
		s = s[1 : len(s)-1]
	}
	if strings.HasSuffix(s, ".") { // ONE trailing root dot on the domain; net/mail rejects it
		s = s[:len(s)-1]
	}
	if s == "" || len(s) > maxAddressBytes {
		return "", ErrInvalidAddress
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= 0x20 || c >= 0x7f {
			return "", ErrInvalidAddress
		}
	}
	parsed, err := mail.ParseAddress(s)
	if err != nil || parsed.Name != "" || parsed.Address != s {
		return "", ErrInvalidAddress // display names, groups, quoting and comments are not keys
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 || strings.Count(s, "@") != 1 {
		return "", ErrInvalidAddress
	}
	local, domain := s[:at], strings.ToLower(s[at+1:])
	if !validLocal(local) || !validDomain(domain) {
		return "", ErrInvalidAddress
	}
	return strings.ToLower(local) + "@" + domain, nil
}

// validLocal accepts an RFC 5322 dot-atom: atext characters separated by single
// dots, no leading or trailing dot.
func validLocal(l string) bool {
	if l == "" || len(l) > maxLocalBytes || l[0] == '.' || l[len(l)-1] == '.' || strings.Contains(l, "..") {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' ||
			strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0
		if !ok {
			return false
		}
	}
	return true
}

func validDomain(d string) bool {
	if d == "" || len(d) > maxDomainBytes {
		return false
	}
	labels := strings.Split(d, ".")
	if last := labels[len(labels)-1]; strings.Trim(last, "0123456789") == "" {
		return false // an all-numeric last label is never a TLD: this is an IP-address-looking name
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
