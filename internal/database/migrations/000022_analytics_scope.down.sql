UPDATE api_keys SET scopes = array_remove(scopes, 'analytics:read');
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check CHECK (
    scopes <@ ARRAY[
        'emails:send','emails:read','domains:read','domains:write',
        'webhooks:read','webhooks:write','suppressions:read','suppressions:write',
        'templates:read','templates:write','contacts:read','contacts:write',
        'audiences:read','audiences:write','broadcasts:read','broadcasts:write'
    ]::text[]
);
