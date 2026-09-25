// Package database's GDPR subject-request surface (v0.46). Operator-only
// (matches v0.19's precedent for tenant/key management: no self-service
// endpoint) tooling to answer "what do you know about this address" and
// "delete what you know about this address" for one recipient within one
// tenant.
//
// Scope, deliberately narrow: deletes the contacts row (which cascades to
// audience_members - see migration 000018), every recipients row for that
// address, and every broadcast_recipients row for that address (see
// DEC-209: broadcast_recipients keeps its OWN independent snapshot of a
// contact - email/name/attributes captured at broadcast-creation time -
// that is not reachable through the contacts FK at all, so leaving it
// alone would let a pending/materialized snapshot still get sent to an
// address erasure just reported as deleted). It does NOT delete the parent
// messages/events/delivery_attempts - those are the tenant's own
// operational/delivery records naming multiple parties (a message can have
// other recipients), not solely "this person's data" in the way a contact
// record is, and remain subject to the ordinary retention purge (see
// retention.go) instead. It also does NOT touch suppressions, for the same
// legitimate-interest reason suppressions already survive a message purge:
// removing a suppression on erasure would let MailX re-email someone who
// complained or hard-bounced.
package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdmail "net/mail"
	"time"

	"github.com/Ferousco-dev/mailx/internal/contact"
)

// GDPRRecipientRecord is one message this subject received, from the
// recipients table (never message content - see the package doc for why
// message bodies/events are out of scope here).
type GDPRRecipientRecord struct {
	MessageID string
	Address   string // as stored (not normalized) - the actual data held
	Status    string
	CreatedAt time.Time
}

// GDPRBroadcastSnapshot is one broadcast_recipients row matching the
// subject - its OWN independent snapshot (email/name/attributes captured
// at broadcast-creation time), not reachable via the contacts FK, so it
// must be reported separately from Contact/Recipients or an access
// request would silently omit real personal data MailX still holds - see
// DEC-209/DEC-210.
type GDPRBroadcastSnapshot struct {
	ID          string
	BroadcastID string
	Email       string
	Name        string
	Attributes  map[string]string
	Status      string
	CreatedAt   time.Time
}

// GDPRSubjectData is everything MailX holds about one address within one
// tenant, across the scope this package covers.
type GDPRSubjectData struct {
	Email              string
	Contact            *Contact
	AudienceID         []string
	Recipients         []GDPRRecipientRecord
	BroadcastSnapshots []GDPRBroadcastSnapshot
}

// ExportSubjectData answers a GDPR-style access request: everything MailX
// holds about email within tenantID, across contacts/audience membership/
// recipient history. email need not already be a normalized contact key -
// it is normalized internally the same way contact records are, and
// recipients are matched by decoding each stored address the same way
// (recipients are stored in whatever form the sender used - bracketed,
// bare, or "Name <addr>" - so an exact-string match would silently miss
// real matches; see cmd/mailx/submission.go's normalizeMailboxKey for the
// same class of problem, fixed the same way here).
func (db *DB) ExportSubjectData(ctx context.Context, tenantID, email string) (GDPRSubjectData, error) {
	key, err := contact.Normalize(email)
	if err != nil {
		return GDPRSubjectData{}, fmt.Errorf("database: %w", err)
	}
	out := GDPRSubjectData{Email: key}

	c, err := db.getContactByNormalizedEmail(ctx, tenantID, key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return GDPRSubjectData{}, err
	}
	if err == nil {
		out.Contact = &c
		rows, err := db.pool.Query(ctx, `SELECT audience_id FROM audience_members WHERE tenant_id = $1 AND contact_id = $2`, tenantID, c.ID)
		if err != nil {
			return GDPRSubjectData{}, fmt.Errorf("database: load audience membership: %w", normalizeErr(err))
		}
		for rows.Next() {
			var aid string
			if err := rows.Scan(&aid); err != nil {
				rows.Close()
				return GDPRSubjectData{}, err
			}
			out.AudienceID = append(out.AudienceID, aid)
		}
		if err := rows.Err(); err != nil {
			return GDPRSubjectData{}, err
		}
		rows.Close()
	}

	out.Recipients, err = db.findRecipientsByAddress(ctx, tenantID, key)
	if err != nil {
		return GDPRSubjectData{}, err
	}
	out.BroadcastSnapshots, err = db.findBroadcastSnapshotsByAddress(ctx, tenantID, key)
	if err != nil {
		return GDPRSubjectData{}, err
	}
	return out, nil
}

// GDPRDeleteResult reports what erasure actually removed.
type GDPRDeleteResult struct {
	ContactDeleted             bool
	RecipientsDeleted          int
	BroadcastRecipientsDeleted int
}

// DeleteSubjectData erases what ExportSubjectData would have reported -
// see the package doc for exactly what is and is not in scope.
func (db *DB) DeleteSubjectData(ctx context.Context, tenantID, email string) (GDPRDeleteResult, error) {
	key, err := contact.Normalize(email)
	if err != nil {
		return GDPRDeleteResult{}, fmt.Errorf("database: %w", err)
	}

	recipients, err := db.findRecipientsByAddress(ctx, tenantID, key)
	if err != nil {
		return GDPRDeleteResult{}, err
	}
	broadcastSnapshots, err := db.findBroadcastSnapshotsByAddress(ctx, tenantID, key)
	if err != nil {
		return GDPRDeleteResult{}, err
	}
	broadcastRecipientIDs := make([]string, len(broadcastSnapshots))
	for i, s := range broadcastSnapshots {
		broadcastRecipientIDs[i] = s.ID
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return GDPRDeleteResult{}, fmt.Errorf("database: begin gdpr delete: %w", normalizeErr(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var out GDPRDeleteResult
	for _, r := range recipients {
		if _, err := tx.Exec(ctx, `DELETE FROM recipients WHERE message_id = $1 AND address = $2`, r.MessageID, r.Address); err != nil {
			return GDPRDeleteResult{}, fmt.Errorf("database: delete recipient row: %w", normalizeErr(err))
		}
		out.RecipientsDeleted++
	}

	// broadcast_recipients keeps its own independent snapshot of a
	// contact (email/name/attributes), NOT reachable via the contacts FK -
	// see the package doc / DEC-209. Deleted outright (not scrubbed), same
	// hard-delete policy already chosen for contacts/recipients above; this
	// does reduce a broadcast's own historical sent-count analytics for an
	// already-sent row, accepted as the simpler, more clearly-compliant
	// tradeoff over keeping a scrubbed row around for counting purposes.
	if len(broadcastRecipientIDs) > 0 {
		tag, err := tx.Exec(ctx, `DELETE FROM broadcast_recipients WHERE id = ANY($1)`, broadcastRecipientIDs)
		if err != nil {
			return GDPRDeleteResult{}, fmt.Errorf("database: delete broadcast_recipients row: %w", normalizeErr(err))
		}
		out.BroadcastRecipientsDeleted = int(tag.RowsAffected())
	}

	tag, err := tx.Exec(ctx, `DELETE FROM contacts WHERE tenant_id = $1 AND normalized_email = $2`, tenantID, key)
	if err != nil {
		return GDPRDeleteResult{}, fmt.Errorf("database: delete contact: %w", normalizeErr(err))
	}
	out.ContactDeleted = tag.RowsAffected() > 0

	if err := tx.Commit(ctx); err != nil {
		return GDPRDeleteResult{}, fmt.Errorf("database: commit gdpr delete: %w", normalizeErr(err))
	}
	return out, nil
}

func (db *DB) getContactByNormalizedEmail(ctx context.Context, tenantID, normalizedEmail string) (Contact, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT `+contactColumns+` FROM contacts WHERE tenant_id = $1 AND normalized_email = $2`,
		tenantID, normalizedEmail,
	)
	return scanContact(row)
}

// findRecipientsByAddress scans this tenant's recipients (joined through
// messages for tenant scoping) and keeps only rows whose address matches
// normalizedEmail once decoded the same way - see the package/type docs
// for why an exact-string match is not sufficient here.
//
// Known limitation (RSK-041): this is a full scan of the tenant's
// recipients, decoded row-by-row in Go - acceptable for this operator-only
// CLI path today, but could exceed the CLI's fixed context deadline on a
// tenant with a very large recipient history. A real fix needs an indexed
// normalized-address column populated at INSERT time (messages.go);
// deferred as a separate, larger change.
func (db *DB) findRecipientsByAddress(ctx context.Context, tenantID, normalizedEmail string) ([]GDPRRecipientRecord, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT r.message_id, r.address, r.status, r.created_at
		FROM recipients r
		JOIN messages m ON m.id = r.message_id
		WHERE m.tenant_id = $1`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("database: scan recipients for gdpr match: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []GDPRRecipientRecord
	for rows.Next() {
		var r GDPRRecipientRecord
		if err := rows.Scan(&r.MessageID, &r.Address, &r.Status, &r.CreatedAt); err != nil {
			return nil, err
		}
		if key, ok := decodeRecipientAddress(r.Address); ok && key == normalizedEmail {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// findBroadcastSnapshotsByAddress scans this tenant's broadcast_recipients
// (its email column is a plain address snapshot, not bracket/display-name
// form like recipients.address, but still run through the same normalize
// step for a correct case-insensitive match against key) and returns every
// full row whose email matches - used by BOTH ExportSubjectData (must
// report these, see GDPRBroadcastSnapshot's doc) and DeleteSubjectData
// (deletes them by ID), one query instead of two divergent ones.
func (db *DB) findBroadcastSnapshotsByAddress(ctx context.Context, tenantID, normalizedEmail string) ([]GDPRBroadcastSnapshot, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, broadcast_id, email, name, attributes, status, created_at
		FROM broadcast_recipients WHERE tenant_id = $1`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("database: scan broadcast_recipients for gdpr match: %w", normalizeErr(err))
	}
	defer rows.Close()
	var out []GDPRBroadcastSnapshot
	for rows.Next() {
		var s GDPRBroadcastSnapshot
		var attrs []byte
		if err := rows.Scan(&s.ID, &s.BroadcastID, &s.Email, &s.Name, &attrs, &s.Status, &s.CreatedAt); err != nil {
			return nil, err
		}
		key, err := contact.Normalize(s.Email)
		if err != nil || key != normalizedEmail {
			continue
		}
		if len(attrs) > 0 {
			if err := json.Unmarshal(attrs, &s.Attributes); err != nil {
				return nil, fmt.Errorf("database: decode broadcast_recipients attributes: %w", err)
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// decodeRecipientAddress extracts the bare mailbox from a stored recipient
// address, in any of the forms MailX has ever written there - bracketed
// ("<addr>"), bare ("addr"), or display-name ("Name <addr>") - then runs
// it through contact.Normalize for the same domain-lowercasing contact
// identities use. contact.Normalize alone rejects display-name input by
// design (a contact KEY must be exact), which is exactly why this can't
// call it directly on the raw stored value: a recipient row legitimately
// carries a display name, a contact key never should.
func decodeRecipientAddress(raw string) (string, bool) {
	parsed, err := stdmail.ParseAddress(raw)
	if err != nil {
		return "", false
	}
	key, err := contact.Normalize(parsed.Address)
	if err != nil {
		return "", false
	}
	return key, true
}
