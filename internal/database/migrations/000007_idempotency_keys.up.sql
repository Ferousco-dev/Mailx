-- v0.20 HTTP idempotency: lets a developer safely retry an uncertain
-- POST /v1/emails (response lost, timeout, etc.) without risking a second
-- logical email. PostgreSQL is the sole authority — never Redis, never a
-- process-local map — so this survives restarts and works correctly
-- across multiple MailX instances without any distributed lock.
--
-- (tenant_id, operation, idempotency_key) is the natural key and IS the
-- primary key: nothing else in MailX needs to reference one of these rows
-- by a separate synthetic id, so a composite PK is simpler than adding
-- one. operation is a stable, Go-internals-independent identifier (e.g.
-- "emails.create"), not a route/handler name, so it can outlive
-- refactors and one day cover more than one endpoint without collision.
--
-- fingerprint is a SHA-256 of the canonical validated request (see
-- internal/idempotency's doc) — it is what lets MailX tell "the exact
-- same retried request" apart from "a different request that happens to
-- reuse this key", which must be rejected, not silently replayed.
--
-- resource_id is nullable: it is set only once the owning request
-- durably commits the message/outbox it created (see
-- internal/database.InsertMessage's idempotency-completion doc) - a row
-- stuck at status='in_progress' with resource_id still NULL means its
-- owning request crashed before finishing, which is exactly the
-- condition ClaimIdempotencyKey's staleness reclaim looks for.
CREATE TABLE idempotency_keys (
    tenant_id       TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    operation       TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    fingerprint     TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'in_progress'
                    CHECK (status IN ('in_progress', 'completed')),
    resource_id     TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, operation, idempotency_key)
);

-- Query: "find expired rows to clean up, oldest first, bounded batch" —
-- the only query besides the PK lookup this table serves. Without this
-- index, cleanup would sequentially scan the entire table (including
-- every still-active row) on every run.
CREATE INDEX idx_idempotency_keys_expires_at ON idempotency_keys (expires_at);
