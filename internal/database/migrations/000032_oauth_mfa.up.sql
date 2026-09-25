-- v0.47 phase 3b: OAuth (Google/GitHub) sign-in and TOTP MFA for human
-- accounts. See .ilana/decisions.md DEC-228..DEC-231.

-- TOTP secrets are sealed with internal/secretbox under MAILX_MFA_MASTER_KEY
-- (associated data "mfa:<human_id>"); plaintext secrets are never stored.
-- The pending pair holds an enrolled-but-unconfirmed secret; confirm moves it
-- into the active pair and sets mfa_enabled. mfa_last_used_step blocks replay
-- of an already-accepted TOTP code within its validity window.
ALTER TABLE humans
    ADD COLUMN mfa_enabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN mfa_secret_ciphertext BYTEA,
    ADD COLUMN mfa_secret_nonce BYTEA,
    ADD COLUMN mfa_pending_secret_ciphertext BYTEA,
    ADD COLUMN mfa_pending_secret_nonce BYTEA,
    ADD COLUMN mfa_last_used_step BIGINT,
    ADD CONSTRAINT humans_mfa_enabled_has_secret CHECK (NOT mfa_enabled OR (mfa_secret_ciphertext IS NOT NULL AND mfa_secret_nonce IS NOT NULL));

-- Single-use recovery codes, SHA-256 hashed like every other token here.
CREATE TABLE mfa_backup_codes (
    id         TEXT PRIMARY KEY,
    human_id   TEXT NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL UNIQUE,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX mfa_backup_codes_human_idx ON mfa_backup_codes (human_id);

-- Short-lived, single-use login challenge issued after a correct first
-- factor on an MFA-enabled account. Opaque random token (hash stored), NOT a
-- JWT, so it can never verify as an access token.
CREATE TABLE mfa_challenges (
    id              TEXT PRIMARY KEY,
    human_id        TEXT NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    token_hash      TEXT NOT NULL UNIQUE,
    expires_at      TIMESTAMPTZ NOT NULL,
    used_at         TIMESTAMPTZ,
    failed_attempts INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX mfa_challenges_human_idx ON mfa_challenges (human_id);

-- Links a provider account to a human. One provider account maps to exactly
-- one human; a human may link several providers.
CREATE TABLE human_oauth_identities (
    provider         TEXT NOT NULL CHECK (provider IN ('google','github')),
    provider_user_id TEXT NOT NULL CHECK (length(provider_user_id) BETWEEN 1 AND 255),
    human_id         TEXT NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    email            TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, provider_user_id)
);
CREATE INDEX human_oauth_identities_human_idx ON human_oauth_identities (human_id);

-- OAuth CSRF state: hash of a random value, single-use (DELETE ... RETURNING),
-- 10-minute TTL.
CREATE TABLE oauth_states (
    state_hash TEXT PRIMARY KEY,
    provider   TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
