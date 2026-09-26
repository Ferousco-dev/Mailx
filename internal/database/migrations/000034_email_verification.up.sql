-- Email verification for human accounts (DEC-241..243).
-- NULL = unverified (same nullable-timestamp convention as last_login_at).
-- Existing accounts stay NULL: verification gates nothing in this pass
-- (DEC-243), so no backfill is needed.
ALTER TABLE humans ADD COLUMN email_verified_at TIMESTAMPTZ;

-- Same shape as password_reset_tokens (migration 000029): only a hash is
-- stored, single use, short-lived (15-minute TTL enforced by
-- humanauth.EmailVerificationTokenTTL, not a CHECK).
CREATE TABLE email_verification_tokens (
    id          TEXT PRIMARY KEY,
    human_id    TEXT NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_email_verification_tokens_hash ON email_verification_tokens (token_hash);
-- Used by the issue path's "supersede my other pending tokens" update.
CREATE INDEX idx_email_verification_tokens_human ON email_verification_tokens (human_id) WHERE used_at IS NULL;
