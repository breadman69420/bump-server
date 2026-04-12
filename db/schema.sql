-- Bump server database schema

CREATE TABLE IF NOT EXISTS reports (
    id BIGSERIAL PRIMARY KEY,
    reporter_hash VARCHAR(32) NOT NULL,
    reported_hash VARCHAR(32) NOT NULL,
    reason VARCHAR(20) NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_reports_reported ON reports(reported_hash);
CREATE UNIQUE INDEX IF NOT EXISTS idx_reports_unique ON reports(reporter_hash, reported_hash);

CREATE TABLE IF NOT EXISTS session_log (
    id BIGSERIAL PRIMARY KEY,
    device_hash VARCHAR(32) NOT NULL,
    requested_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_session_device ON session_log(device_hash, requested_at);

-- Blocklist derived from reports (3+ reports)
CREATE TABLE IF NOT EXISTS blocklist (
    device_hash VARCHAR(32) PRIMARY KEY,
    blocked_at TIMESTAMPTZ DEFAULT NOW()
);

-- Verified purchase tokens (replay protection)
CREATE TABLE IF NOT EXISTS verified_purchases (
    purchase_token VARCHAR(512) PRIMARY KEY,
    device_hash VARCHAR(32) NOT NULL,
    product_id VARCHAR(64) NOT NULL,
    platform VARCHAR(10) NOT NULL DEFAULT 'google',
    verified_at TIMESTAMPTZ DEFAULT NOW()
);

-- Paid bump balance per device, persistent across app reinstalls and
-- local data clears. Free daily bumps are tracked in Redis (daily: key
-- with midnight UTC TTL). Paid balance is decremented here only after
-- the free daily allowance is exhausted.
CREATE TABLE IF NOT EXISTS device_bumps (
    device_hash VARCHAR(32) PRIMARY KEY,
    paid_balance INTEGER NOT NULL DEFAULT 0 CHECK (paid_balance >= 0),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Add platform column to verified_purchases (safe for existing databases)
DO $$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'verified_purchases' AND column_name = 'platform'
    ) THEN
        ALTER TABLE verified_purchases ADD COLUMN platform VARCHAR(10) NOT NULL DEFAULT 'google';
    END IF;
END $$;

-- Automatic cleanup: delete session logs older than 7 days
-- Run via pg_cron or application-level scheduled task
-- DELETE FROM session_log WHERE requested_at < NOW() - INTERVAL '7 days';
