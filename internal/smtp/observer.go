package smtp

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Observer receives privacy-safe session events. It gets a process-local
// session ID (no security meaning) and bounded categories only: never
// addresses, headers, or message content. Implementations must not block;
// a panic in an observer is swallowed and cannot affect the SMTP session.
type Observer interface {
	SessionStarted(sessionID string)
	// SessionEnded reports failed=true for a transport failure (a reply could
	// not be written); a client hanging up is a normal completion.
	SessionEnded(sessionID string, duration time.Duration, failed bool)
	// SessionRejected reports a connection refused at the concurrency cap.
	SessionRejected()
	// MessageResult result is accepted|rejected|temporary_failure; reason is a
	// bounded category (oversized, malformed, content, sink_failed, or "").
	MessageResult(sessionID, result, reason string)
}

func observe(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "sess_" + hex.EncodeToString(b[:])
}

func (c *connection) message(result, reason string) {
	if o := c.config.Observer; o != nil {
		observe(func() { o.MessageResult(c.id, result, reason) })
	}
}
