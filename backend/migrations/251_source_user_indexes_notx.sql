CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_source_user_created
    ON usage_logs ((source_user->>'source_id'), (source_user->>'user_id'), created_at DESC)
    WHERE source_user IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ops_source_user_created
    ON ops_error_logs ((source_user->>'source_id'), (source_user->>'user_id'), created_at DESC)
    WHERE source_user IS NOT NULL;
