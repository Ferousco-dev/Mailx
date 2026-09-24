// Package database's analytics surface (v0.38) derives tenant-facing
// sending analytics from facts MailX ALREADY records durably — the
// `events` table (one row per lifecycle transition: queued, delivered,
// deferred, bounced, failed, suppressed, complained, each timestamped by
// occurred_at) and, for a current-state snapshot, `suppressions`. No new
// aggregate/derived table: `idx_events_tenant_occurred (tenant_id,
// occurred_at DESC, id DESC)` already supports every query here (see
// EXPLAIN evidence in analytics_test.go), and MailX's write-heavy sending
// path gets zero new writes from this milestone.
//
// Metric vocabulary (see also PublicEventType and .ilana/architecture.md):
//   - Queued: message accepted BY MAILX (durable acceptance, the 202
//     boundary) — never inbox placement.
//   - Delivered: remote SMTP server accepted final DATA (2xx) — this is
//     "SMTP acceptance", NOT inbox placement, spam-folder avoidance, or a
//     human reading the message. MailX cannot observe any of those.
//   - Deferred: a temporary failure occurred on an attempt; the message may
//     still succeed on retry (its LATEST fact, e.g. Delivered/Failed, is
//     what should be trusted for final outcome — Deferred is historical).
//   - Bounced: reused for BOTH a synchronous hard-bounce-shaped rejection
//     and v0.32's asynchronous DSN feedback (see EventBounced's doc) — an
//     earlier Queued/Delivered fact for the SAME message is not erased by a
//     later Bounced fact; both remain true and both are counted.
//   - Failed: a permanent (non-retriable) delivery failure.
//   - Suppressed: this specific send was skipped because the recipient was
//     ALREADY suppressed at send time — a historical fact about that one
//     send, distinct from CurrentlySuppressed below.
//   - Complained: v0.32 async complaint feedback matched a recipient of
//     this message. Like Bounced, does not erase an earlier accepted fact.
//   - CurrentlySuppressed (overview only, not timeseries): the COUNT of
//     currently-active suppression entries for the tenant right now — a
//     snapshot of current policy state, not a historical/time-bucketed
//     fact, and deliberately not mixed into the same counters as the above.
//
// Every count above is a query over `events`/`suppressions` filtered to
// [from, to) (half-open — see AnalyticsTimeRange) and tenant_id. A message
// can contribute to multiple counters (e.g. Queued=1 AND later Bounced=1)
// because these are not mutually exclusive lifecycle facts — forcing them
// into one bucket would destroy real history (see the package doc above).
package database

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AnalyticsMaxRange bounds how much history one overview/timeseries request
// may span, so a request can never force an unbounded aggregate scan.
const AnalyticsMaxRange = 90 * 24 * time.Hour

// AnalyticsMaxBuckets bounds timeseries response size (e.g. 90 days hourly
// would be ~2160 buckets — still under this, but a careless multi-year
// hourly request is rejected rather than silently returning tens of
// thousands of points).
const AnalyticsMaxBuckets = 744 // 31 days of hourly buckets, or ~2 years of daily

// ErrTooManyBuckets means the requested range/interval combination would
// produce more than AnalyticsMaxBuckets timeseries points.
var ErrTooManyBuckets = errors.New("database: analytics range/interval produces too many buckets")

// AnalyticsCounts is one set of event-derived counts — the shared shape
// between the overview and each timeseries bucket. Field meanings are
// documented in this file's package doc.
type AnalyticsCounts struct {
	Queued     int
	Delivered  int
	Deferred   int
	Bounced    int
	Failed     int
	Suppressed int
	Complained int
	Opened     int
	Clicked    int
}

// AnalyticsOverview is AnalyticsCounts across [From, To) plus the one
// current-state snapshot value (CurrentlySuppressed) this milestone
// exposes — see the package doc for why that is not folded into the
// time-bucketed counts.
type AnalyticsOverview struct {
	From, To            time.Time
	Counts              AnalyticsCounts
	CurrentlySuppressed int
}

// eventCountsQuery is shared by AnalyticsOverview and AnalyticsTimeseries:
// tenant-scoped, [from,to) half-open, GROUP BY event_type. Callers add
// their own SELECT prefix/GROUP BY suffix.
const eventTypeCountColumns = `
	count(*) FILTER (WHERE event_type = 'queued')     AS queued,
	count(*) FILTER (WHERE event_type = 'delivered')  AS delivered,
	count(*) FILTER (WHERE event_type = 'deferred')   AS deferred,
	count(*) FILTER (WHERE event_type = 'bounced')    AS bounced,
	count(*) FILTER (WHERE event_type = 'failed')     AS failed,
	count(*) FILTER (WHERE event_type = 'suppressed') AS suppressed,
	count(*) FILTER (WHERE event_type = 'complained') AS complained,
	count(*) FILTER (WHERE event_type = 'opened')     AS opened,
	count(*) FILTER (WHERE event_type = 'clicked')    AS clicked`

// AnalyticsOverview aggregates event-derived counts for tenantID across the
// half-open range [from, to) in ONE indexed query (idx_events_tenant_occurred
// covers tenant_id + occurred_at), plus a second bounded query for the
// current suppression count. Never touches message/recipient content.
func (db *DB) AnalyticsOverview(ctx context.Context, tenantID string, from, to time.Time) (AnalyticsOverview, error) {
	if tenantID == "" {
		return AnalyticsOverview{}, errors.New("database: tenant ID is empty")
	}
	if !to.After(from) {
		return AnalyticsOverview{}, errors.New("database: to must be after from")
	}
	out := AnalyticsOverview{From: from, To: to}
	err := db.pool.QueryRow(ctx, `
		SELECT`+eventTypeCountColumns+`
		FROM events
		WHERE tenant_id = $1 AND occurred_at >= $2 AND occurred_at < $3`,
		tenantID, from, to,
	).Scan(&out.Counts.Queued, &out.Counts.Delivered, &out.Counts.Deferred,
		&out.Counts.Bounced, &out.Counts.Failed, &out.Counts.Suppressed, &out.Counts.Complained,
		&out.Counts.Opened, &out.Counts.Clicked)
	if err != nil {
		return AnalyticsOverview{}, fmt.Errorf("database: analytics overview: %w", normalizeErr(err))
	}
	if err := db.pool.QueryRow(ctx,
		`SELECT count(*) FROM suppressions WHERE tenant_id = $1`, tenantID,
	).Scan(&out.CurrentlySuppressed); err != nil {
		return AnalyticsOverview{}, fmt.Errorf("database: analytics current suppressions: %w", normalizeErr(err))
	}
	return out, nil
}

// AnalyticsBucket is one timeseries point. Timestamp is the bucket's start
// (UTC, per the requested interval) — see AnalyticsTimeseries's doc for
// empty-bucket and ordering semantics.
type AnalyticsBucket struct {
	Timestamp time.Time
	Counts    AnalyticsCounts
}

// analyticsIntervals whitelists the only SQL date_trunc units this package
// will ever pass through — never derived from unwhitelisted user input,
// even though date_trunc's unit argument is an ordinary bound parameter
// (not string-interpolated), defense in depth against a future caller
// forwarding a raw query-string value here.
var analyticsIntervals = map[string]bool{"hour": true, "day": true}

// AnalyticsTimeseries buckets event-derived counts for tenantID across
// [from, to) by interval ("hour" or "day", UTC). Buckets are returned in
// ascending Timestamp order; an interval with NO events in it is simply
// ABSENT from the result (sparse, not zero-filled) — callers building a
// chart are expected to fill gaps with zero themselves, since MailX has no
// way to know a tenant's preferred display range beyond [from,to) and
// zero-filling here would risk silently producing an unbounded response
// for a huge range with an accidentally tiny interval (see
// AnalyticsMaxBuckets below, which still rejects that case outright).
func (db *DB) AnalyticsTimeseries(ctx context.Context, tenantID string, from, to time.Time, interval string) ([]AnalyticsBucket, error) {
	if tenantID == "" {
		return nil, errors.New("database: tenant ID is empty")
	}
	if !to.After(from) {
		return nil, errors.New("database: to must be after from")
	}
	if !analyticsIntervals[interval] {
		return nil, fmt.Errorf("database: unsupported analytics interval %q", interval)
	}
	// Reject an excessive range/interval combination BEFORE running the
	// query, computed from the request alone — checking only the RETURNED
	// row count would miss a huge range that happens to have no events in
	// it yet (e.g. a future range), still needing rejection since the
	// point is bounding what the request COULD return, not what it did.
	unit := time.Hour
	if interval == "day" {
		unit = 24 * time.Hour
	}
	if to.Sub(from)/unit > AnalyticsMaxBuckets {
		return nil, ErrTooManyBuckets
	}
	rows, err := db.pool.Query(ctx, `
		SELECT date_trunc($4, occurred_at) AS bucket,`+eventTypeCountColumns+`
		FROM events
		WHERE tenant_id = $1 AND occurred_at >= $2 AND occurred_at < $3
		GROUP BY bucket
		ORDER BY bucket
		LIMIT $5`,
		tenantID, from, to, interval, AnalyticsMaxBuckets+1)
	if err != nil {
		return nil, fmt.Errorf("database: analytics timeseries: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []AnalyticsBucket
	for rows.Next() {
		var b AnalyticsBucket
		if err := rows.Scan(&b.Timestamp, &b.Counts.Queued, &b.Counts.Delivered, &b.Counts.Deferred,
			&b.Counts.Bounced, &b.Counts.Failed, &b.Counts.Suppressed, &b.Counts.Complained,
			&b.Counts.Opened, &b.Counts.Clicked); err != nil {
			return nil, fmt.Errorf("database: scan analytics bucket: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > AnalyticsMaxBuckets {
		return nil, ErrTooManyBuckets
	}
	return out, nil
}

// BroadcastAnalytics is v0.36's DURABLE recipient/orchestration facts
// (never live Audience membership, which may have changed since — see
// broadcast_recipients' snapshot semantics), plus the delivery-outcome
// breakdown for the subset that reached materialization. Intended is the
// full snapshotted recipient count; it is NOT "how many will be delivered"
// — some may still be suppressed/pending/failed. See v0.36: Broadcast
// "completed" means orchestration completed, never that every recipient
// was delivered.
type BroadcastAnalytics struct {
	BroadcastID     string
	Intended        int // total snapshotted broadcast_recipients rows
	Pending         int
	Suppressed      int // suppressed at broadcast-materialization time (recipient-level historical fact)
	Materialized    int
	RecipientFailed int // broadcast_recipients.status = 'failed' (bounded-retry terminal, see migration 000021)
	// Delivered/Bounced/Complained/Failed below are the events-table
	// breakdown for materialized recipients' resulting messages — the
	// SAME semantics as AnalyticsCounts' fields, scoped to this broadcast.
	Delivered  int
	Bounced    int
	Complained int
	Failed     int
}

// BroadcastAnalytics loads recipient-status counts from broadcast_recipients
// (the durable snapshot — never live Audience membership) and, for
// materialized recipients, the events-table delivery-outcome breakdown for
// their resulting messages. tenantID-scoped via the broadcast row itself,
// so an unknown/foreign broadcast id is ErrNotFound.
func (db *DB) BroadcastAnalytics(ctx context.Context, tenantID, broadcastID string) (BroadcastAnalytics, error) {
	var one int
	if err := db.pool.QueryRow(ctx, `SELECT 1 FROM broadcasts WHERE tenant_id = $1 AND id = $2`, tenantID, broadcastID).Scan(&one); err != nil {
		return BroadcastAnalytics{}, fmt.Errorf("database: check broadcast: %w", normalizeErr(err))
	}
	out := BroadcastAnalytics{BroadcastID: broadcastID}
	err := db.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE status = 'suppressed'),
			count(*) FILTER (WHERE status = 'materialized'),
			count(*) FILTER (WHERE status = 'failed'),
			count(*)
		FROM broadcast_recipients WHERE broadcast_id = $1`,
		broadcastID,
	).Scan(&out.Pending, &out.Suppressed, &out.Materialized, &out.RecipientFailed, &out.Intended)
	if err != nil {
		return BroadcastAnalytics{}, fmt.Errorf("database: broadcast recipient counts: %w", normalizeErr(err))
	}
	err = db.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE e.event_type = 'delivered'),
			count(*) FILTER (WHERE e.event_type = 'bounced'),
			count(*) FILTER (WHERE e.event_type = 'complained'),
			count(*) FILTER (WHERE e.event_type = 'failed')
		FROM events e
		WHERE e.tenant_id = $1 AND e.message_id IN (
			SELECT message_id FROM broadcast_recipients WHERE broadcast_id = $2 AND message_id IS NOT NULL
		)`,
		tenantID, broadcastID,
	).Scan(&out.Delivered, &out.Bounced, &out.Complained, &out.Failed)
	if err != nil {
		return BroadcastAnalytics{}, fmt.Errorf("database: broadcast delivery outcomes: %w", normalizeErr(err))
	}
	return out, nil
}

// AnalyticsMaxDomains bounds how many per-domain rows DomainBreakdown
// returns, so a tenant sending to thousands of distinct domains cannot
// force an unbounded response — same posture as AnalyticsMaxBuckets.
const AnalyticsMaxDomains = 50

// DomainBreakdown is one recipient-domain's outcome counts across
// [from, to), scoped by recipients.created_at (recipients are created in
// the same transaction as their message, so this is equivalent to
// filtering by send time without needing a join-time column on
// recipients). Counts reflect the CURRENT per-recipient status (pending/
// delivered/failed/suppressed) — see the recipients table's status
// semantics — not the full historical event stream used elsewhere in this
// file, so a recipient that is later retried and delivered is counted
// once, under its current outcome.
type DomainBreakdown struct {
	Domain     string
	Total      int
	Delivered  int
	Failed     int
	Suppressed int
	Pending    int
}

// DomainBreakdown aggregates recipient outcomes for tenantID across
// [from, to) by the recipient address's domain (case-folded), joined
// through messages for tenant scoping. Ordered by Total descending, capped
// at AnalyticsMaxDomains rows.
func (db *DB) DomainBreakdown(ctx context.Context, tenantID string, from, to time.Time) ([]DomainBreakdown, error) {
	if tenantID == "" {
		return nil, errors.New("database: tenant ID is empty")
	}
	if !to.After(from) {
		return nil, errors.New("database: to must be after from")
	}
	rows, err := db.pool.Query(ctx, `
		SELECT
			lower(trim(trailing '>' from split_part(r.address, '@', 2))) AS domain,
			count(*) AS total,
			count(*) FILTER (WHERE r.status = 'delivered')  AS delivered,
			count(*) FILTER (WHERE r.status = 'failed')     AS failed,
			count(*) FILTER (WHERE r.status = 'suppressed') AS suppressed,
			count(*) FILTER (WHERE r.status = 'pending')    AS pending
		FROM recipients r
		JOIN messages m ON m.id = r.message_id
		WHERE m.tenant_id = $1 AND r.created_at >= $2 AND r.created_at < $3
		GROUP BY domain
		ORDER BY total DESC
		LIMIT $4`,
		tenantID, from, to, AnalyticsMaxDomains,
	)
	if err != nil {
		return nil, fmt.Errorf("database: domain breakdown: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []DomainBreakdown
	for rows.Next() {
		var d DomainBreakdown
		if err := rows.Scan(&d.Domain, &d.Total, &d.Delivered, &d.Failed, &d.Suppressed, &d.Pending); err != nil {
			return nil, fmt.Errorf("database: scan domain breakdown row: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
