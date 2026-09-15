-- v0.18: the public API lets a developer submit the same address in more
-- than one role (e.g. to:[alice] cc:[alice]), which is valid and useful
-- (some mail clients treat To/Cc differently). The old UNIQUE(message_id,
-- address) rejected that outright. header_kind now becomes the API's
-- authoritative role for API-submitted messages (previously a best-effort
-- classification of SMTP-received mail), so uniqueness moves to
-- (message_id, address, header_kind). Two NULL header_kind rows for the
-- same address (a real hidden-Bcc scenario with no matching header) are
-- still both permitted, since SQL UNIQUE treats NULLs as distinct.
ALTER TABLE recipients DROP CONSTRAINT recipients_message_id_address_key;
ALTER TABLE recipients ADD CONSTRAINT recipients_message_id_address_role_key
    UNIQUE (message_id, address, header_kind);
