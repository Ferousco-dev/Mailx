UPDATE api_keys SET scopes = array_remove(array_remove(scopes, 'broadcasts:read'), 'broadcasts:write');
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check CHECK (
    scopes <@ ARRAY[
        'emails:send','emails:read','domains:read','domains:write',
        'webhooks:read','webhooks:write','suppressions:read','suppressions:write',
        'templates:read','templates:write','contacts:read','contacts:write',
        'audiences:read','audiences:write'
    ]::text[]
);

DROP TABLE IF EXISTS broadcast_recipients;
DROP TABLE IF EXISTS broadcasts;
