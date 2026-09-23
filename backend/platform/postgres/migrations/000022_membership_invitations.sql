-- Команда организации через одноразовые коды приглашения (ADR 0045,
-- GAP-API-008). У MVP нет почтовой инфраструктуры, поэтому приглашение — это
-- код, который владелец передаёт сотруднику сам; в базе хранится только его
-- SHA-256. Приглашение не удаляется: принятие и отзыв фиксируются отметками.
CREATE TABLE membership_invitations (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('OWNER', 'MANAGER')),
    code_hash CHAR(64) NOT NULL,
    note TEXT,
    created_by_user_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    accepted_by_user_id UUID,
    revoked_at TIMESTAMPTZ,
    revoked_by_user_id UUID,
    CONSTRAINT membership_invitations_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT membership_invitations_code_unique UNIQUE (code_hash),
    CONSTRAINT membership_invitations_creator_fk
        FOREIGN KEY (tenant_id, created_by_user_id) REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT membership_invitations_acceptor_fk
        FOREIGN KEY (tenant_id, accepted_by_user_id) REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT membership_invitations_revoker_fk
        FOREIGN KEY (tenant_id, revoked_by_user_id) REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT membership_invitations_code_valid CHECK (code_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT membership_invitations_note_valid CHECK (
        note IS NULL OR (note = btrim(note) AND char_length(note) BETWEEN 1 AND 500)
    ),
    CONSTRAINT membership_invitations_lifetime_valid CHECK (expires_at > created_at),
    CONSTRAINT membership_invitations_accept_consistent CHECK (
        (accepted_at IS NULL) = (accepted_by_user_id IS NULL)
    ),
    CONSTRAINT membership_invitations_revoke_consistent CHECK (
        (revoked_at IS NULL) = (revoked_by_user_id IS NULL)
    ),
    CONSTRAINT membership_invitations_single_outcome CHECK (accepted_at IS NULL OR revoked_at IS NULL)
);

CREATE INDEX membership_invitations_tenant_idx
    ON membership_invitations(tenant_id, created_at DESC, id DESC);

-- Та же изоляция, что у остальных таблиц с tenant_id (миграция 000020).
ALTER TABLE membership_invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE membership_invitations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON membership_invitations AS PERMISSIVE FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('lidradar.tenant_id', true), '')::uuid
        OR pg_has_role(current_user, 'lidradar_platform', 'MEMBER')
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('lidradar.tenant_id', true), '')::uuid
        OR pg_has_role(current_user, 'lidradar_platform', 'MEMBER')
    );

-- Принимающий приглашение ещё не член организации и не знает её идентификатор:
-- строка находится по хешу кода, заданному только на время транзакции приёма
-- (set_config(..., true)). Политика открывает ровно одну строку с этим хешем.
CREATE POLICY invitation_by_code ON membership_invitations AS PERMISSIVE FOR SELECT
    USING (code_hash = NULLIF(current_setting('lidradar.invitation_code_hash', true), ''));
