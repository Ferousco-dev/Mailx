package mailx

import (
	"fmt"
	"time"
)

type APIError struct {
	Status     int
	Type       string
	Code       string
	Message    string
	RequestID  string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("mailx: %s (%s): %s", e.Type, e.Code, e.Message)
}
