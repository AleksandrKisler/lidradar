CREATE TABLE contacts (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    display_name TEXT,
    phone_normalized TEXT,
    email_normalized TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT contacts_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT contacts_display_name_valid CHECK (
        display_name IS NULL OR (display_name = btrim(display_name) AND char_length(display_name) BETWEEN 1 AND 200)
    ),
    CONSTRAINT contacts_phone_valid CHECK (
        phone_normalized IS NULL OR (phone_normalized = btrim(phone_normalized) AND char_length(phone_normalized) BETWEEN 1 AND 40)
    ),
    CONSTRAINT contacts_email_valid CHECK (
        email_normalized IS NULL OR (
            email_normalized = lower(btrim(email_normalized)) AND char_length(email_normalized) BETWEEN 3 AND 254
        )
    )
);

CREATE INDEX contacts_tenant_idx ON contacts(tenant_id, created_at, id);
CREATE INDEX contacts_phone_idx
    ON contacts(tenant_id, phone_normalized)
    WHERE phone_normalized IS NOT NULL;
CREATE INDEX contacts_email_idx
    ON contacts(tenant_id, email_normalized)
    WHERE email_normalized IS NOT NULL;

CREATE TABLE external_identities (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    contact_id UUID NOT NULL,
    provider TEXT NOT NULL,
    connection_id UUID,
    external_id TEXT NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT external_identities_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT external_identities_contact_fk
        FOREIGN KEY (tenant_id, contact_id)
        REFERENCES contacts(tenant_id, id),
    CONSTRAINT external_identities_connection_fk
        FOREIGN KEY (tenant_id, connection_id, provider)
        REFERENCES channel_connections(tenant_id, id, provider),
    CONSTRAINT external_identity_unique
        UNIQUE NULLS NOT DISTINCT (tenant_id, provider, connection_id, external_id),
    CONSTRAINT external_identities_provider_valid CHECK (
        provider = btrim(provider) AND char_length(provider) BETWEEN 1 AND 100
    ),
    CONSTRAINT external_identities_external_id_valid CHECK (
        external_id = btrim(external_id) AND char_length(external_id) BETWEEN 1 AND 512
    ),
    CONSTRAINT external_identities_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE INDEX external_identity_contact_idx ON external_identities(tenant_id, contact_id);

CREATE TABLE conversations (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    location_id UUID,
    connection_id UUID NOT NULL,
    contact_id UUID NOT NULL,
    external_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'ARCHIVED')),
    first_message_at TIMESTAMPTZ,
    last_message_at TIMESTAMPTZ,
    last_message_direction TEXT CHECK (
        last_message_direction IS NULL OR last_message_direction IN ('INCOMING', 'OUTGOING', 'SYSTEM')
    ),
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT conversations_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT conversations_tenant_connection_unique UNIQUE (tenant_id, id, connection_id),
    CONSTRAINT conversations_location_fk
        FOREIGN KEY (tenant_id, location_id)
        REFERENCES locations(tenant_id, id),
    CONSTRAINT conversations_connection_fk
        FOREIGN KEY (tenant_id, connection_id)
        REFERENCES channel_connections(tenant_id, id),
    CONSTRAINT conversations_contact_fk
        FOREIGN KEY (tenant_id, contact_id)
        REFERENCES contacts(tenant_id, id),
    CONSTRAINT conversation_external_unique UNIQUE (connection_id, external_id),
    CONSTRAINT conversations_external_id_valid CHECK (
        external_id = btrim(external_id) AND char_length(external_id) BETWEEN 1 AND 512
    ),
    CONSTRAINT conversations_message_bounds_consistent CHECK (
        (first_message_at IS NULL AND last_message_at IS NULL AND last_message_direction IS NULL)
        OR
        (first_message_at IS NOT NULL AND last_message_at IS NOT NULL AND last_message_direction IS NOT NULL
         AND first_message_at <= last_message_at)
    )
);

CREATE INDEX conversation_tenant_updated_idx ON conversations(tenant_id, updated_at DESC, id DESC);
CREATE INDEX conversation_contact_idx ON conversations(tenant_id, contact_id);

CREATE TABLE messages (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    conversation_id UUID NOT NULL,
    connection_id UUID NOT NULL,
    external_id TEXT NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('INCOMING', 'OUTGOING', 'SYSTEM')),
    type TEXT NOT NULL CHECK (type IN ('TEXT', 'IMAGE', 'VOICE', 'AUDIO', 'VIDEO', 'DOCUMENT', 'OTHER')),
    text TEXT,
    sender_external_id TEXT,
    reply_to_message_id UUID,
    sent_at TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    provider_deleted_at TIMESTAMPTZ,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT messages_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT messages_tenant_conversation_unique UNIQUE (tenant_id, id, conversation_id),
    CONSTRAINT messages_conversation_connection_fk
        FOREIGN KEY (tenant_id, conversation_id, connection_id)
        REFERENCES conversations(tenant_id, id, connection_id),
    CONSTRAINT messages_reply_fk
        FOREIGN KEY (tenant_id, reply_to_message_id, conversation_id)
        REFERENCES messages(tenant_id, id, conversation_id),
    CONSTRAINT message_external_unique UNIQUE (connection_id, external_id),
    CONSTRAINT messages_external_id_valid CHECK (
        external_id = btrim(external_id) AND char_length(external_id) BETWEEN 1 AND 512
    ),
    CONSTRAINT messages_sender_external_id_valid CHECK (
        sender_external_id IS NULL OR (
            sender_external_id = btrim(sender_external_id) AND char_length(sender_external_id) BETWEEN 1 AND 512
        )
    ),
    CONSTRAINT messages_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE INDEX messages_conversation_time_idx
    ON messages(tenant_id, conversation_id, sent_at DESC, id DESC);

CREATE TABLE attachments (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    message_id UUID NOT NULL,
    object_key TEXT NOT NULL,
    mime_type TEXT,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    sha256 TEXT,
    provider_file_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT attachments_message_fk
        FOREIGN KEY (tenant_id, message_id)
        REFERENCES messages(tenant_id, id)
        ON DELETE CASCADE,
    CONSTRAINT attachments_object_key_valid CHECK (
        object_key = btrim(object_key) AND char_length(object_key) BETWEEN 1 AND 1024
    ),
    CONSTRAINT attachments_sha256_valid CHECK (
        sha256 IS NULL OR sha256 ~ '^[0-9a-f]{64}$'
    )
);

CREATE INDEX attachments_message_idx ON attachments(tenant_id, message_id);
