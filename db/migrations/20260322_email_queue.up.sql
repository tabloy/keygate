CREATE TABLE IF NOT EXISTS email_queue (
    id           TEXT PRIMARY KEY,
    to_addr      TEXT NOT NULL,
    subject      TEXT NOT NULL,
    body         TEXT NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 5,
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    next_retry   TIMESTAMPTZ,
    error        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_email_queue_status ON email_queue(status, next_retry) WHERE status = 'pending';
