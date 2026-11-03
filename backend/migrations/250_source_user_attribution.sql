-- Attribution only; historical identities remain unknown. No backfill or billing changes.
SET LOCAL lock_timeout = '3s';
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS source_user JSONB;
ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS source_user JSONB;
