CREATE TABLE IF NOT EXISTS platform_metadata (
    key TEXT PRIMARY KEY,
    value JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO platform_metadata (key, value)
VALUES ('schema', '{"baseline":1}'::jsonb)
ON CONFLICT (key) DO NOTHING;
