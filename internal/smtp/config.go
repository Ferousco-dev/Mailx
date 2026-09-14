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
	defaultTimeout              = 5 * time.Minute
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
}

func DefaultConfig() Config {
	return Config{
		CommandLineLimit: DefaultCommandLineLimit,
		ReadTimeout:      defaultTimeout,
		WriteTimeout:     defaultTimeout,
		MaxMessageSize:   DefaultMaxMessageSize,
	}
}

func (c Config) normalized() (Config, error) {
	if c.MaxMessageSize == 0 {
		c.MaxMessageSize = DefaultMaxMessageSize
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
	return c, nil
}

func (c Config) validate() error {
	_, err := c.normalized()
	return err
}
