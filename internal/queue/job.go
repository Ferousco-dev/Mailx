package queue

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrEmptyJobID       = errors.New("queue: job ID is empty")
	ErrEmptyMessageID   = errors.New("queue: message ID is empty")
	ErrJobIDControlChar = errors.New("queue: job ID contains forbidden control character")
)

// Job references one unit of work by MessageID only; content and retry
// history live elsewhere (internal/storage, internal/retry). Prefer a
// deterministic ID (e.g. derived from MessageID) so Enqueue is idempotent.
type Job struct {
	ID          string
	MessageID   string
	EnqueuedAt  time.Time
	AvailableAt time.Time
}

func (j Job) validate() error {
	if strings.TrimSpace(j.ID) == "" {
		return ErrEmptyJobID
	}
	if strings.ContainsAny(j.ID, "\r\n\x00") {
		return ErrJobIDControlChar
	}
	if strings.TrimSpace(j.MessageID) == "" {
		return ErrEmptyMessageID
	}
	return nil
}

// NewJobID generates a random job identifier for callers with no natural
// deterministic ID to reuse.
func NewJobID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("queue: generate job ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}
