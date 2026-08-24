CREATE TABLE service_catalog_items (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    location_id UUID,
    name TEXT NOT NULL,
    normalized_name TEXT NOT NULL,
    price_from NUMERIC(14,2),
    price_to NUMERIC(14,2),
    currency CHAR(3) NOT NULL DEFAULT 'RUB',
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT service_catalog_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT service_catalog_location_fk
        FOREIGN KEY (tenant_id, location_id)
        REFERENCES locations(tenant_id, id),
    CONSTRAINT service_catalog_name_valid CHECK (
        name = btrim(name) AND char_length(name) BETWEEN 1 AND 200
    ),
    CONSTRAINT service_catalog_normalized_name_valid CHECK (
        normalized_name = btrim(normalized_name) AND char_length(normalized_name) BETWEEN 1 AND 200
    ),
    CONSTRAINT service_catalog_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT service_catalog_price_from_nonnegative CHECK (price_from IS NULL OR price_from >= 0),
    CONSTRAINT service_catalog_price_to_nonnegative CHECK (price_to IS NULL OR price_to >= 0),
    CONSTRAINT service_catalog_price_range_valid CHECK (
        price_from IS NULL OR price_to IS NULL OR price_from <= price_to
    )
);

CREATE INDEX service_catalog_tenant_active_idx
    ON service_catalog_items(tenant_id, active, normalized_name, id);

CREATE INDEX service_catalog_tenant_location_idx
    ON service_catalog_items(tenant_id, location_id)
    WHERE location_id IS NOT NULL;
