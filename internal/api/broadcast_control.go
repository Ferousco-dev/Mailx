package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Ferousco-dev/mailx/internal/database"
)

func (h *broadcastHandler) handleCancel(w http.ResponseWriter, r *http.Request) {
	b, err := h.db.CancelBroadcast(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"))
	if err != nil {
		writeError(w, r, broadcastControlError(err))
		return
	}
	writeJSON(w, http.StatusOK, broadcastFromRow(b))
}

func (h *broadcastHandler) handleReschedule(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, newError(ErrUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return
	}
	var req struct {
		SendAt *string `json:"send_at"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body is not valid JSON"))
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, r, newError(ErrInvalidRequest, "malformed_json", "request body must contain one JSON object"))
		return
	}
	if req.SendAt == nil {
		writeError(w, r, newError(ErrValidation, "missing_send_at", "send_at is required"))
		return
	}
	sendAt, aerr := parseSendAt(req.SendAt, h.now())
	if aerr != nil {
		writeError(w, r, aerr)
		return
	}
	b, err := h.db.RescheduleBroadcast(r.Context(), tenantFromContext(r.Context()), r.PathValue("id"), *sendAt)
	if err != nil {
		writeError(w, r, broadcastControlError(err))
		return
	}
	writeJSON(w, http.StatusOK, broadcastFromRow(b))
}

func broadcastControlError(err error) *apiError {
	switch {
	case errors.Is(err, database.ErrNotFound):
		return newError(ErrNotFoundType, "broadcast_not_found", "no broadcast found with that id")
	case errors.Is(err, database.ErrBroadcastStateConflict):
		return newError(ErrConflictType, "broadcast_state_conflict", "the broadcast cannot be changed in its current state")
	case errors.Is(err, database.ErrInvalidBroadcastSchedule):
		return newError(ErrValidation, "invalid_send_at", "send_at must be in the future")
	default:
		return newError(ErrInternal, "internal_error", "failed to update broadcast")
	}
}
