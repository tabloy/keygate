-- Floating license sessions
CREATE TABLE IF NOT EXISTS floating_sessions (
    id          TEXT PRIMARY KEY,
    license_id  TEXT NOT NULL REFERENCES licenses(id) ON DELETE CASCADE,
    identifier  TEXT NOT NULL,
    label       TEXT NOT NULL DEFAULT '',
    ip_address  TEXT NOT NULL DEFAULT '',
    checked_out TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    heartbeat   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(license_id, identifier)
);
CREATE INDEX IF NOT EXISTS idx_floating_license ON floating_sessions(license_id);
CREATE INDEX IF NOT EXISTS idx_floating_expires ON floating_sessions(expires_at);

-- Plans: floating license support
ALTER TABLE plans ADD COLUMN IF NOT EXISTS license_model TEXT NOT NULL DEFAULT 'standard'
    CHECK (license_model IN ('standard', 'floating'));
ALTER TABLE plans ADD COLUMN IF NOT EXISTS floating_timeout INTEGER NOT NULL DEFAULT 30;
    -- minutes before a session expires without heartbeat

-- Add-on features (purchasable on top of base plan)
CREATE TABLE IF NOT EXISTS addons (
    id          TEXT PRIMARY KEY,
    product_id  TEXT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    feature     TEXT NOT NULL,
    value_type  TEXT NOT NULL CHECK (value_type IN ('bool', 'int', 'string', 'quota')),
    value       TEXT NOT NULL,
    quota_period TEXT NOT NULL DEFAULT '',
    quota_unit  TEXT NOT NULL DEFAULT '',
    active      BOOLEAN NOT NULL DEFAULT true,
    sort_order  INTEGER NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(product_id, slug)
);
CREATE INDEX IF NOT EXISTS idx_addons_product ON addons(product_id);

-- License add-ons (which addons a license has purchased)
CREATE TABLE IF NOT EXISTS license_addons (
    id          TEXT PRIMARY KEY,
    license_id  TEXT NOT NULL REFERENCES licenses(id) ON DELETE CASCADE,
    addon_id    TEXT NOT NULL REFERENCES addons(id) ON DELETE CASCADE,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(license_id, addon_id)
);
CREATE INDEX IF NOT EXISTS idx_license_addons_license ON license_addons(license_id);

-- Metered billing records (synced to payment provider)
CREATE TABLE IF NOT EXISTS metered_billing (
    id              TEXT PRIMARY KEY,
    license_id      TEXT NOT NULL REFERENCES licenses(id) ON DELETE CASCADE,
    feature         TEXT NOT NULL,
    quantity        BIGINT NOT NULL,
    period_key      TEXT NOT NULL,
    synced          BOOLEAN NOT NULL DEFAULT false,
    synced_at       TIMESTAMPTZ,
    external_id     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(license_id, feature, period_key)
);
CREATE INDEX IF NOT EXISTS idx_metered_license ON metered_billing(license_id);
CREATE INDEX IF NOT EXISTS idx_metered_unsynced ON metered_billing(synced) WHERE synced = false;
