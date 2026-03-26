CREATE TABLE IF NOT EXISTS processed_events (
    id         TEXT PRIMARY KEY,
    provider   TEXT NOT NULL,
    event_id   TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(provider, event_id)
);
CREATE INDEX IF NOT EXISTS idx_processed_events_lookup ON processed_events(provider, event_id);
