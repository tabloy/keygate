CREATE TABLE IF NOT EXISTS notifications (
    id         TEXT PRIMARY KEY,
    license_id TEXT NOT NULL REFERENCES licenses(id) ON DELETE CASCADE,
    tag        TEXT NOT NULL,
    sent_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(license_id, tag)
);
CREATE INDEX IF NOT EXISTS idx_notifications_license ON notifications(license_id);
