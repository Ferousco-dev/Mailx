ALTER TABLE recipients DROP CONSTRAINT recipients_message_id_address_role_key;
ALTER TABLE recipients ADD CONSTRAINT recipients_message_id_address_key
    UNIQUE (message_id, address);
