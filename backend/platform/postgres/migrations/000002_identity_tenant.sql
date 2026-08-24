CREATE TABLE users (
    id UUID PRIMARY KEY,
    email TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    display_name TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'DISABLED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT users_email_canonical CHECK (email = lower(btrim(email))),
    CONSTRAINT users_email_unique UNIQUE (email)
);

CREATE TABLE sessions (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash CHAR(64) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    ip INET,
    user_agent TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ,
    CONSTRAINT sessions_token_unique UNIQUE (token_hash)
);

CREATE INDEX sessions_user_idx ON sessions(user_id);
CREATE INDEX sessions_expiry_idx ON sessions(expires_at) WHERE revoked_at IS NULL;

CREATE TABLE organizations (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    default_timezone TEXT NOT NULL,
    default_currency CHAR(3) NOT NULL DEFAULT 'RUB',
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'SUSPENDED', 'ARCHIVED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('OWNER', 'MANAGER')),
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'INVITED', 'DISABLED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT membership_user_tenant_unique UNIQUE (tenant_id, user_id)
);

CREATE INDEX memberships_user_idx ON memberships(user_id);

CREATE TABLE locations (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    timezone TEXT NOT NULL,
    response_threshold_minutes INTEGER NOT NULL DEFAULT 45
        CHECK (response_threshold_minutes BETWEEN 1 AND 1440),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT locations_tenant_id_unique UNIQUE (tenant_id, id)
);

CREATE INDEX locations_tenant_idx ON locations(tenant_id, active, created_at, id);

CREATE TABLE location_business_hours (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL,
    location_id UUID NOT NULL,
    weekday SMALLINT NOT NULL CHECK (weekday BETWEEN 1 AND 7),
    is_closed BOOLEAN NOT NULL DEFAULT FALSE,
    opens_at TIME,
    closes_at TIME,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT business_hours_location_fk
        FOREIGN KEY (tenant_id, location_id)
        REFERENCES locations(tenant_id, id)
        ON DELETE CASCADE,
    CONSTRAINT business_hours_day_unique UNIQUE (tenant_id, location_id, weekday),
    CONSTRAINT business_hours_consistency CHECK (
        (is_closed = TRUE AND opens_at IS NULL AND closes_at IS NULL)
        OR
        (is_closed = FALSE AND opens_at IS NOT NULL AND closes_at IS NOT NULL AND opens_at < closes_at)
    )
);

CREATE INDEX business_hours_tenant_location_idx
    ON location_business_hours(tenant_id, location_id, weekday);
