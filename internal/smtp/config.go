package smtp

import (
	"fmt"
	"time"
)

const (
	// DefaultCommandLineLimit includes the terminating CRLF, per RFC 5321.
	DefaultCommandLineLimit = 512
	// DefaultMaxMessageSize is 10 MiB of canonicalized SMTP DATA content.
	DefaultMaxMessageSize int64 = 10 * 1024 * 1024
	// DefaultMaxConnections bounds active SMTP sessions per server instance.
	DefaultMaxConnections = 100
	// DefaultMaxRecipients caps RCPT TO entries per transaction, per RFC 5321.
	DefaultMaxRecipients = 100
	defaultTimeout       = 5 * time.Minute
)

// Config contains per-server transport safety settings. A zero timeout
// disables its corresponding deadline. A zero MaxMessageSize selects the
// finite default rather than disabling message-size protection.
type Config struct {
	CommandLineLimit int
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	// MaxMessageSize counts canonical message bytes after dot unstuffing,
	// including stored CRLF endings and excluding the DATA terminator.
	MaxMessageSize int64
	// MaxConnections limits concurrently active SMTP connection handlers.
	// Zero selects DefaultMaxConnections.
	MaxConnections int
	// MaxRecipients caps RCPT TO entries in one transaction. Zero selects
	// DefaultMaxRecipients.
	MaxRecipients int
}

func DefaultConfig() Config {
	return Config{
		CommandLineLimit: DefaultCommandLineLimit,
		ReadTimeout:      defaultTimeout,
		WriteTimeout:     defaultTimeout,
		MaxMessageSize:   DefaultMaxMessageSize,
		MaxConnections:   DefaultMaxConnections,
		MaxRecipients:    DefaultMaxRecipients,
	}
}

func (c Config) normalized() (Config, error) {
	if c.MaxMessageSize == 0 {
		c.MaxMessageSize = DefaultMaxMessageSize
	}
	if c.MaxConnections == 0 {
		c.MaxConnections = DefaultMaxConnections
	}
	if c.MaxRecipients == 0 {
		c.MaxRecipients = DefaultMaxRecipients
	}
	if c.CommandLineLimit < DefaultCommandLineLimit {
		return Config{}, fmt.Errorf("SMTP command line limit must be at least %d octets including CRLF", DefaultCommandLineLimit)
	}
	if c.ReadTimeout < 0 || c.WriteTimeout < 0 {
		return Config{}, fmt.Errorf("SMTP timeouts must not be negative")
	}
	if c.MaxMessageSize < 0 {
		return Config{}, fmt.Errorf("SMTP maximum message size must not be negative")
	}
	if c.MaxConnections < 0 {
		return Config{}, fmt.Errorf("SMTP maximum connections must not be negative")
	}
	if c.MaxRecipients < 0 {
		return Config{}, fmt.Errorf("SMTP maximum recipients must not be negative")
	}
	return c, nil
}

func (c Config) validate() error {
	_, err := c.normalized()
	return err
}
