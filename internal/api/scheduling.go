package api

import "time"

// parseSendAt is the one place the "send_at" wire field (normal emails and
// Broadcasts, v0.37) is parsed: a strict RFC 3339 absolute instant, never in
// the past. nil in means immediate (unchanged pre-v0.37 behavior) and
// returns a nil time back out — callers treat nil as "no scheduling".
func parseSendAt(raw *string, now time.Time) (*time.Time, *apiError) {
	if raw == nil {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		return nil, newError(ErrValidation, "invalid_send_at", "send_at must be an RFC 3339 timestamp")
	}
	if t.Before(now) {
		return nil, newError(ErrValidation, "invalid_send_at", "send_at must not be in the past")
	}
	return &t, nil
}
