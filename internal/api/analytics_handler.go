package api

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

// analyticsHandler exposes read-only, tenant-scoped sending analytics
// derived from existing durable facts (internal/database/analytics.go) —
// it never becomes a second delivery-truth source and never writes
// anything. See that file's package doc for the exact metric vocabulary.
type analyticsHandler struct {
	db  *database.DB
	now func() time.Time
}

// analyticsCounts is the wire shape of database.AnalyticsCounts.
type analyticsCounts struct {
	Queued     int `json:"queued"`
	Delivered  int `json:"delivered"`
	Deferred   int `json:"deferred"`
	Bounced    int `json:"bounced"`
	Failed     int `json:"failed"`
	Suppressed int `json:"suppressed"`
	Complained int `json:"complained"`
}

func countsFromRow(c database.AnalyticsCounts) analyticsCounts {
	return analyticsCounts{
		Queued: c.Queued, Delivered: c.Delivered, Deferred: c.Deferred,
		Bounced: c.Bounced, Failed: c.Failed, Suppressed: c.Suppressed, Complained: c.Complained,
	}
}

type analyticsOverviewResponse struct {
	From                string          `json:"from"`
	To                  string          `json:"to"`
	Counts              analyticsCounts `json:"counts"`
	CurrentlySuppressed int             `json:"currently_suppressed"`
}

// parseAnalyticsRange validates the shared from/to/range query contract:
// both required, RFC 3339, to > from, and the span bounded by
// database.AnalyticsMaxRange so a request can never force an unbounded
// aggregate scan.
func parseAnalyticsRange(r *http.Request) (from, to time.Time, aerr *apiError) {
	fromRaw, toRaw := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if fromRaw == "" || toRaw == "" {
		return time.Time{}, time.Time{}, newError(ErrValidation, "missing_range", "from and to are both required (RFC 3339)")
	}
	from, err := time.Parse(time.RFC3339, fromRaw)
	if err != nil {
		return time.Time{}, time.Time{}, newError(ErrValidation, "invalid_from", "from must be an RFC 3339 timestamp")
	}
	to, err = time.Parse(time.RFC3339, toRaw)
	if err != nil {
		return time.Time{}, time.Time{}, newError(ErrValidation, "invalid_to", "to must be an RFC 3339 timestamp")
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, newError(ErrValidation, "invalid_range", "to must be after from")
	}
	if to.Sub(from) > database.AnalyticsMaxRange {
		return time.Time{}, time.Time{}, newError(ErrValidation, "range_too_large",
			fmt.Sprintf("the requested range must not exceed %s", database.AnalyticsMaxRange))
	}
	return from, to, nil
}

// handleOverview implements GET /v1/analytics/overview?from=&to=.
func (h *analyticsHandler) handleOverview(w http.ResponseWriter, r *http.Request) {
	from, to, aerr := parseAnalyticsRange(r)
	if aerr != nil {
		writeError(w, r, aerr)
		return
	}
	tenantID := tenantFromContext(r.Context())
	overview, err := h.db.AnalyticsOverview(r.Context(), tenantID, from, to)
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load analytics overview"))
		return
	}
	writeJSON(w, http.StatusOK, analyticsOverviewResponse{
		From: overview.From.UTC().Format(time.RFC3339), To: overview.To.UTC().Format(time.RFC3339),
		Counts: countsFromRow(overview.Counts), CurrentlySuppressed: overview.CurrentlySuppressed,
	})
}

type analyticsBucketResponse struct {
	Timestamp string          `json:"timestamp"`
	Counts    analyticsCounts `json:"counts"`
}

// handleTimeseries implements GET /v1/analytics/timeseries?from=&to=&interval=hour|day.
func (h *analyticsHandler) handleTimeseries(w http.ResponseWriter, r *http.Request) {
	from, to, aerr := parseAnalyticsRange(r)
	if aerr != nil {
		writeError(w, r, aerr)
		return
	}
	interval := r.URL.Query().Get("interval")
	if interval != "hour" && interval != "day" {
		writeError(w, r, newError(ErrValidation, "invalid_interval", "interval must be \"hour\" or \"day\""))
		return
	}
	tenantID := tenantFromContext(r.Context())
	buckets, err := h.db.AnalyticsTimeseries(r.Context(), tenantID, from, to, interval)
	if errors.Is(err, database.ErrTooManyBuckets) {
		writeError(w, r, newError(ErrValidation, "range_too_large", "the requested range/interval would return too many buckets; use a shorter range or a larger interval"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load analytics timeseries"))
		return
	}
	resp := make([]analyticsBucketResponse, 0, len(buckets))
	for _, b := range buckets {
		resp = append(resp, analyticsBucketResponse{Timestamp: b.Timestamp.UTC().Format(time.RFC3339), Counts: countsFromRow(b.Counts)})
	}
	writeJSON(w, http.StatusOK, resp)
}

type broadcastAnalyticsResponse struct {
	BroadcastID     string `json:"broadcast_id"`
	Intended        int    `json:"intended"`
	Pending         int    `json:"pending"`
	Suppressed      int    `json:"suppressed"`
	Materialized    int    `json:"materialized"`
	RecipientFailed int    `json:"recipient_failed"`
	Delivered       int    `json:"delivered"`
	Bounced         int    `json:"bounced"`
	Complained      int    `json:"complained"`
	Failed          int    `json:"failed"`
}

// handleBroadcast implements GET /v1/analytics/broadcasts/{id}.
func (h *analyticsHandler) handleBroadcast(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	a, err := h.db.BroadcastAnalytics(r.Context(), tenantID, r.PathValue("id"))
	if errors.Is(err, database.ErrNotFound) {
		writeError(w, r, newError(ErrNotFoundType, "broadcast_not_found", "no broadcast found with that id"))
		return
	}
	if err != nil {
		writeError(w, r, newError(ErrInternal, "internal_error", "failed to load broadcast analytics"))
		return
	}
	writeJSON(w, http.StatusOK, broadcastAnalyticsResponse{
		BroadcastID: a.BroadcastID, Intended: a.Intended, Pending: a.Pending, Suppressed: a.Suppressed,
		Materialized: a.Materialized, RecipientFailed: a.RecipientFailed,
		Delivered: a.Delivered, Bounced: a.Bounced, Complained: a.Complained, Failed: a.Failed,
	})
}
