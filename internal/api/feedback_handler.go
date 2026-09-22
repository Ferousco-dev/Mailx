package api

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/feedback"
)

// FeedbackConfig configures the /internal/feedback ingestion route.
type FeedbackConfig struct {
	IngestToken string
	Correlator  *feedback.Correlator
}

// feedbackHandler is the bounded feedback-ingestion boundary (v0.32): NOT a
// tenant-facing /v1 route, NOT general inbound email. It accepts exactly one
// thing — feedback about a message MailX already sent — from an
// operator-trusted caller (a bounce mailbox relay or provider webhook adapter
// the operator runs; see docs/design-v0.32.md). Tenant is always resolved
// server-side from the correlated message, never from the request.
type feedbackHandler struct {
	db          *database.DB
	correlator  *feedback.Correlator
	ingestToken string // constant-time compared; empty disables the route (see routes.go)
	now         func() time.Time
}

type feedbackRequest struct {
	Type             string  `json:"type"` // "dsn" | "complaint"
	MessageID        string  `json:"message_id,omitempty"`
	CorrelationToken string  `json:"correlation_token,omitempty"`
	Raw              string  `json:"raw,omitempty"`       // base64 DSN bytes, type=dsn
	Recipient        string  `json:"recipient,omitempty"` // type=complaint
	ReceivedAt       *string `json:"received_at,omitempty"`
}

func (h *feedbackHandler) authenticate(r *http.Request) bool {
	if h.ingestToken == "" {
		return false
	}
	raw := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) {
		return false
	}
	got := []byte(strings.TrimPrefix(raw, prefix))
	want := []byte(h.ingestToken)
	return len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1
}

func (h *feedbackHandler) handleIngest(w http.ResponseWriter, r *http.Request) {
	if !h.authenticate(r) {
		writeError(w, r, newError(ErrAuthentication, "invalid_ingest_token", "invalid or missing feedback ingestion credential"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, feedback.MaxInputBytes+4096)
	var req feedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}

	messageID, ok := h.resolveMessageID(req)
	if !ok {
		writeError(w, r, newError(ErrInvalidRequest, "unresolvable_correlation", "neither correlation_token nor message_id resolved to a message"))
		return
	}

	now := h.now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	receivedAt := now()
	if req.ReceivedAt != nil {
		if t, err := time.Parse(time.RFC3339, *req.ReceivedAt); err == nil {
			receivedAt = t
		}
	}

	var items []feedback.Feedback
	switch req.Type {
	case "dsn":
		raw, err := base64.StdEncoding.DecodeString(req.Raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "invalid_raw", "raw must be base64"))
			return
		}
		groups, err := feedback.ParseDSN(raw)
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "unparseable_dsn", "the submission is not a recognizable delivery-status report"))
			return
		}
		for _, g := range groups {
			fb, err := feedback.FromDSN(g, raw, receivedAt)
			if err != nil {
				continue // no Final-Recipient on this group: skip it, not the whole batch
			}
			items = append(items, fb)
		}
	case "complaint":
		fb, err := feedback.FromComplaint(req.Recipient, receivedAt, []byte(req.Recipient+messageID+receivedAt.String()))
		if err != nil {
			writeError(w, r, newError(ErrInvalidRequest, "missing_recipient", "recipient is required for a complaint report"))
			return
		}
		items = append(items, fb)
	default:
		writeError(w, r, newError(ErrInvalidRequest, "invalid_type", `type must be "dsn" or "complaint"`))
		return
	}

	processed := 0
	for _, fb := range items {
		if _, err := h.db.ProcessFeedback(r.Context(), messageID, fb); err != nil {
			if errors.Is(err, database.ErrNotFound) || errors.Is(err, database.ErrRecipientNotFound) {
				continue // this group didn't correlate to a real recipient; skip, not fatal
			}
			writeError(w, r, newError(ErrInternal, "internal_error", "failed to durably record feedback"))
			return
		}
		processed++
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"processed": processed})
}

// resolveMessageID prefers a Correlator-verified token (unforgeable) and falls
// back to a bare message_id, which is safe here ONLY because this whole route
// already requires the operator ingestion credential (see docs/design-v0.32.md
// "Correlation" for why a public route could not make the same trade-off).
func (h *feedbackHandler) resolveMessageID(req feedbackRequest) (string, bool) {
	if req.CorrelationToken != "" && h.correlator != nil {
		if id, ok := h.correlator.Verify(req.CorrelationToken); ok {
			return id, true
		}
		return "", false
	}
	if req.MessageID != "" {
		return req.MessageID, true
	}
	return "", false
}
