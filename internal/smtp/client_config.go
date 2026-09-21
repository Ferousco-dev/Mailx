package smtp

import (
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultReplyLineLimit follows RFC 5321 §4.5.3.1.5: 512 octets including CRLF.
	DefaultReplyLineLimit = 512
	// DefaultReplyLineCount caps multiline reply length to a defensible bound.
	DefaultReplyLineCount = 128
	defaultClientTimeout  = 5 * time.Minute
	defaultDialTimeout    = 30 * time.Second
	defaultClientIdentity = "localhost"
)

// ClientConfig configures per-instance timeouts and the EHLO identity. All
// fields are read-only after construction so Client.Send is safe to call from
// multiple goroutines concurrently.
type ClientConfig struct {
	// Identity is the EHLO/HELO argument. Must not contain CR, LF, NUL or
	// spaces. Empty selects "localhost".
	Identity string
	// DialTimeout bounds the TCP connect. Zero selects a finite default.
	DialTimeout time.Duration
	// ReadTimeout bounds each SMTP reply read. Zero selects the default.
	ReadTimeout time.Duration
	// WriteTimeout bounds each SMTP write. Zero selects the default.
	WriteTimeout time.Duration
	// MaxReplyLineBytes caps one SMTP reply line including CRLF. Zero
	// selects DefaultReplyLineLimit.
	MaxReplyLineBytes int
	// MaxReplyLines caps the number of continuation lines in one reply.
	// Zero selects DefaultReplyLineCount.
	MaxReplyLines int
	// TLS configures outbound STARTTLS. The zero value is opportunistic TLS
	// with certificate verification and a finite handshake timeout.
	TLS TLSConfig
	// AuthObserver receives one bounded event per Send that carried
	// credentials. Nil disables it; a panicking observer cannot affect delivery.
	AuthObserver AuthObserver
}

// DefaultClientConfig returns the finite default configuration.
func DefaultClientConfig() ClientConfig {
	c, _ := ClientConfig{}.normalized()
	return c
}

func (c ClientConfig) normalized() (ClientConfig, error) {
	if c.Identity == "" {
		c.Identity = defaultClientIdentity
	}
	if strings.ContainsAny(c.Identity, "\r\n\x00 \t") {
		return ClientConfig{}, fmt.Errorf("SMTP client identity contains forbidden characters")
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = defaultDialTimeout
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = defaultClientTimeout
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = defaultClientTimeout
	}
	if c.MaxReplyLineBytes == 0 {
		c.MaxReplyLineBytes = DefaultReplyLineLimit
	}
	if c.MaxReplyLines == 0 {
		c.MaxReplyLines = DefaultReplyLineCount
	}
	if c.TLS.HandshakeTimeout == 0 {
		c.TLS.HandshakeTimeout = defaultTLSHandshakeTimeout
	}
	if c.TLS.HandshakeTimeout < 0 {
		return ClientConfig{}, fmt.Errorf("SMTP TLS handshake timeout must not be negative")
	}
	if c.TLS.Policy != TLSOpportunistic && c.TLS.Policy != TLSRequired {
		return ClientConfig{}, fmt.Errorf("SMTP TLS policy is not recognized")
	}
	if c.DialTimeout < 0 || c.ReadTimeout < 0 || c.WriteTimeout < 0 {
		return ClientConfig{}, fmt.Errorf("SMTP client timeouts must not be negative")
	}
	if c.MaxReplyLineBytes < DefaultReplyLineLimit {
		return ClientConfig{}, fmt.Errorf("SMTP client reply line limit must be at least %d octets", DefaultReplyLineLimit)
	}
	if c.MaxReplyLines <= 0 {
		return ClientConfig{}, fmt.Errorf("SMTP client reply line count must be positive")
	}
	return c, nil
}
