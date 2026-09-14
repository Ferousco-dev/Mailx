package smtp

import "strings"

// serializeForDATA normalizes bare LF to CRLF, dot-stuffs any line beginning
// with '.', and appends the SMTP DATA terminator ".\r\n" per RFC 5321 §4.5.2.
// An empty raw message is transmitted as just the terminator.
func serializeForDATA(raw string) string {
	// 1) Normalize line endings to CRLF without doubling existing CRLFs.
	var b strings.Builder
	b.Grow(len(raw) + 16)
	i := 0
	for i < len(raw) {
		c := raw[i]
		if c == '\r' {
			if i+1 < len(raw) && raw[i+1] == '\n' {
				b.WriteByte('\r')
				b.WriteByte('\n')
				i += 2
				continue
			}
			b.WriteByte('\r')
			b.WriteByte('\n')
			i++
			continue
		}
		if c == '\n' {
			b.WriteByte('\r')
			b.WriteByte('\n')
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	normalized := b.String()
	if normalized != "" && !strings.HasSuffix(normalized, "\r\n") {
		normalized += "\r\n"
	}

	// 2) Dot-stuff any line beginning with '.'.
	var out strings.Builder
	out.Grow(len(normalized) + 16)
	lineStart := true
	for i := 0; i < len(normalized); i++ {
		if lineStart && normalized[i] == '.' {
			out.WriteByte('.')
		}
		out.WriteByte(normalized[i])
		lineStart = normalized[i] == '\n'
	}
	out.WriteString(".\r\n")
	return out.String()
}
