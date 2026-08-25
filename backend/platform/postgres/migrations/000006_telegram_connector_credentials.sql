ALTER TABLE channel_connections
    ADD CONSTRAINT channel_connections_encrypted_credentials_size_valid
    CHECK (
        encrypted_credentials IS NULL
        OR octet_length(encrypted_credentials) BETWEEN 1 AND 8192
    );
