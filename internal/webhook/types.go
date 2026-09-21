// Package webhook implements MailX's tenant-managed, signed, at-least-once
// HTTP event delivery. It never changes email lifecycle truth.
package webhook

import (
	"errors"
	"sort"
)

const (
	EventQueued          = "email.queued"
	EventDelivered       = "email.delivered"
	EventDeliveryDelayed = "email.delivery_delayed"
	EventFailed          = "email.failed"
	EventBounced         = "email.bounced"
)

var EventTypes = []string{EventQueued, EventDelivered, EventDeliveryDelayed, EventFailed, EventBounced}

func normalizeEventTypes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("webhook: at least one event type is required")
	}
	known := make(map[string]bool, len(EventTypes))
	for _, value := range EventTypes {
		known[value] = true
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, value := range in {
		if !known[value] {
			return nil, errors.New("webhook: unknown event type " + value)
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out, nil
}
