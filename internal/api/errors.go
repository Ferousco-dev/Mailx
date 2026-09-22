package api

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// ErrorType is the small, stable public error taxonomy every /v1 error
// belongs to. Adding a value is a considered API change; the HTTP status
// each maps to is fixed by errorStatus below, never chosen ad hoc at the
// call site.
type ErrorType string

const (
	ErrInvalidRequest         ErrorType = "invalid_request"
	ErrValidation             ErrorType = "validation_error"
	ErrAuthentication         ErrorType = "authentication_error"
	ErrForbidden              ErrorType = "forbidden"
	ErrNotFoundType           ErrorType = "not_found"
	ErrConflictType           ErrorType = "conflict"
	ErrPayloadTooLarge        ErrorType = "payload_too_large"
	ErrUnsupportedMediaType   ErrorType = "unsupported_media_type"
	ErrInternal               ErrorType = "internal_error"
	ErrTemporarilyUnavailable ErrorType = "temporarily_unavailable"
	ErrRateLimited            ErrorType = "rate_limited"
)

var errorStatus = map[ErrorType]int{
	ErrInvalidRequest:         http.StatusBadRequest,
	ErrValidation:             http.StatusUnprocessableEntity,
	ErrAuthentication:         http.StatusUnauthorized,
	ErrForbidden:              http.StatusForbidden,
	ErrNotFoundType:           http.StatusNotFound,
	ErrConflictType:           http.StatusConflict,
	ErrPayloadTooLarge:        http.StatusRequestEntityTooLarge,
	ErrUnsupportedMediaType:   http.StatusUnsupportedMediaType,
	ErrInternal:               http.StatusInternalServerError,
	ErrTemporarilyUnavailable: http.StatusServiceUnavailable,
	ErrRateLimited:            http.StatusTooManyRequests,
}

// apiError is a handler-raised error carrying everything writeError needs.
// code is a short machine-readable detail within Type (e.g.
// "invalid_recipient"); message is safe to show a developer verbatim —
// never a raw SQL/Redis/filesystem error.
type apiError struct {
	Type    ErrorType
	Code    string
	Message string
	// RetryAfter, when positive, is sent as a Retry-After header in whole seconds
	// (RFC 9110 10.2.3) on 429 and 503 refusals.
	RetryAfter int
}

func (e *apiError) Error() string { return e.Message }

func newError(t ErrorType, code, message string) *apiError {
	return &apiError{Type: t, Code: code, Message: message}
}

// errorBody is the exact wire shape of every /v1 error response.
type errorBody struct {
	Error struct {
		Type      ErrorType `json:"type"`
		Code      string    `json:"code"`
		Message   string    `json:"message"`
		RequestID string    `json:"request_id"`
	} `json:"error"`
}

// defaultUnavailableRetryAfter is the Retry-After (seconds) of a 503 that names none.
const defaultUnavailableRetryAfter = 5

func writeError(w http.ResponseWriter, r *http.Request, err *apiError) {
	status, ok := errorStatus[err.Type]
	if !ok {
		status = http.StatusInternalServerError
	}
	var body errorBody
	body.Error.Type = err.Type
	body.Error.Code = err.Code
	body.Error.Message = err.Message
	body.Error.RequestID = requestIDFromContext(r.Context())

	w.Header().Set("Content-Type", "application/json")
	retryAfter := err.RetryAfter
	if retryAfter <= 0 && status == http.StatusServiceUnavailable {
		// Every 503 carries Retry-After, as the OpenAPI contract states.
		retryAfter = defaultUnavailableRetryAfter
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
