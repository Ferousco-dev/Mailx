-- v0.47 (Phase 1) follow-up: track when a human account was last used to
-- log in. NULL means "never logged in" - a signed-up account that has not
-- yet completed a Login call (SignUp mints a session directly but is not
-- itself a "login" for this column's purpose, matching how most products
-- distinguish "account created" from "signed in").
ALTER TABLE humans ADD COLUMN last_login_at TIMESTAMPTZ;
