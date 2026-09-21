package dkim

import (
	"bytes"
	"strings"
)

// relaxedHeader implements RFC 6376 3.4.2 for one header field given as it
// appears in the message (name, colon, value, possibly folded; no trailing
// CRLF): lowercase the name, unfold, collapse whitespace runs to one space,
// trim, and remove whitespace around the colon.
func relaxedHeader(field string) string {
	name, value, _ := strings.Cut(field, ":")
	value = strings.NewReplacer("\r\n", "").Replace(value)
	return strings.ToLower(strings.TrimRight(name, " \t")) + ":" + collapseWSP(strings.Trim(value, " \t"))
}

// collapseWSP reduces every run of spaces and tabs to a single space.
func collapseWSP(s string) string {
	var b strings.Builder
	inWSP := false
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			if !inWSP {
				b.WriteByte(' ')
			}
			inWSP = true
			continue
		}
		inWSP = false
		b.WriteByte(s[i])
	}
	return b.String()
}

// relaxedBody implements RFC 6376 3.4.4: strip trailing whitespace from every
// line, collapse whitespace runs to one space, drop empty lines at the end of
// the body, and terminate a non-empty body with CRLF.
func relaxedBody(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	lines := bytes.Split(body, []byte("\r\n"))
	out := make([][]byte, 0, len(lines))
	for _, l := range lines {
		out = append(out, []byte(collapseWSP(strings.TrimRight(string(l), " \t"))))
	}
	for len(out) > 0 && len(out[len(out)-1]) == 0 {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return nil
	}
	return append(bytes.Join(out, []byte("\r\n")), '\r', '\n')
}
