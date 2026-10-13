-- Регистрация по приглашениям (ADR 0057). Код платформы выпускает оператор
-- командой platform-admin invite; он даёт право зарегистрироваться и создать
-- одну организацию. В базе хранится только SHA-256 кода, строка не удаляется:
-- погашение и отзыв фиксируются отметками. Таблицей владеет модуль admin,
-- погашает код при регистрации модуль identity в одной транзакции с созданием
-- пользователя.
CREATE TABLE platform_invitations (
    id UUID PRIMARY KEY,
    code_hash CHAR(64) NOT NULL,
    note TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    consumed_at TIMESTAMPTZ,
    consumed_by_user_id UUID,
    CONSTRAINT platform_invitations_code_unique UNIQUE (code_hash),
    CONSTRAINT platform_invitations_consumer_fk
        FOREIGN KEY (consumed_by_user_id) REFERENCES users(id),
    CONSTRAINT platform_invitations_code_valid CHECK (code_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT platform_invitations_note_valid CHECK (
        note IS NULL OR (note = btrim(note) AND char_length(note) BETWEEN 1 AND 500)
    ),
    CONSTRAINT platform_invitations_lifetime_valid CHECK (expires_at > created_at),
    CONSTRAINT platform_invitations_consume_consistent CHECK (
        (consumed_at IS NULL) = (consumed_by_user_id IS NULL)
    ),
    CONSTRAINT platform_invitations_single_outcome CHECK (consumed_at IS NULL OR revoked_at IS NULL)
);

CREATE INDEX platform_invitations_created_idx
    ON platform_invitations(created_at DESC, id DESC);

-- Право создать организации в режиме invite_only. Код платформы даёт единицу,
-- создание организации тратит её условным UPDATE в своей транзакции, команда
-- оператора allow-organization добавляет. Существующие пользователи получают
-- ноль: в открытом режиме право не проверяется.
ALTER TABLE users
    ADD COLUMN organization_rights SMALLINT NOT NULL DEFAULT 0
        CONSTRAINT users_organization_rights_valid CHECK (organization_rights BETWEEN 0 AND 100);
