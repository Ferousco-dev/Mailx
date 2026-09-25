-- v0.47 (Phase 1) follow-up: self-service password reset for human
-- accounts. Mirrors refresh_tokens' shape (only a hash is stored, single
-- use, short-lived) rather than reusing that table directly - a password
-- reset token authorizes a completely different, much more dangerous
-- action (changing the account's credential) than a refresh token
-- (continuing an already-established session), and conflating the two
-- would make it too easy for a future change to one to accidentally widen
-- the other's blast radius.
CREATE TABLE password_reset_tokens (
    id          TEXT PRIMARY KEY,
    human_id    TEXT NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL,
    -- 5-minute TTL is enforced by the application (humanauth.PasswordResetTokenTTL),
    -- not a CHECK here (the DB has no reliable notion of "now" at insert
    -- time relative to what the application intends) - expires_at is
    -- simply whatever the application computed.
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Hot path: "verify by hash" on every reset-password call.
CREATE UNIQUE INDEX idx_password_reset_tokens_hash ON password_reset_tokens (token_hash);
-- "does this human have an outstanding unused reset token" - not currently
-- queried by name, but matches refresh_tokens' analogous partial index for
-- the same future-proofing reason (e.g. a future "invalidate my other
-- pending reset requests" step).
CREATE INDEX idx_password_reset_tokens_human ON password_reset_tokens (human_id) WHERE used_at IS NULL;
