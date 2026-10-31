-- Nullable metadata only: historical rows remain unknown; no backfill or index.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS upstream_request_body JSONB;
COMMENT ON COLUMN usage_logs.upstream_request_body IS
    'Codex OAuth HTTP body bytes prepared per attempt, including retries; excludes responses and network overhead';
