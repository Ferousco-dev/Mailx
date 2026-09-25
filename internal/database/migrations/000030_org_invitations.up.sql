-- v0.47 (Phase 1) follow-up: organization invitations, plus the URL-only
-- avatar/logo fields the invite email needs. Per the operator's decisions:
-- invite by email only (no separate invite-code UX), owner-only sending,
-- 5-hour single-use tokens (same hashed-token shape as password_reset_tokens),
-- and images are plain nullable URL columns (no upload pipeline yet).

ALTER TABLE tenants ADD COLUMN logo_url TEXT;
ALTER TABLE humans ADD COLUMN avatar_url TEXT;

CREATE TABLE org_invitations (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    invited_by  TEXT NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    -- Case-insensitive matching follows humans.normalized_email's
    -- convention; raw_email is kept for display in operator/API responses.
    normalized_email TEXT NOT NULL CHECK (length(normalized_email) BETWEEN 3 AND 254 AND normalized_email !~ '\s'),
    raw_email   TEXT NOT NULL,
    token_hash  TEXT NOT NULL,
    -- 5-hour TTL is enforced by the application (humanauth.OrgInvitationTTL),
    -- same convention as password_reset_tokens.expires_at.
    expires_at  TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Hot path: "verify by hash" on accept.
CREATE UNIQUE INDEX idx_org_invitations_hash ON org_invitations (token_hash);
-- "does this tenant already have a pending invite for this email" -
-- queried before sending a new one to avoid duplicate outstanding invites.
CREATE INDEX idx_org_invitations_tenant_email ON org_invitations (tenant_id, normalized_email) WHERE accepted_at IS NULL;
