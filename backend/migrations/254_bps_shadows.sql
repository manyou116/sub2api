ALTER TABLE accounts DROP CONSTRAINT IF EXISTS chk_accounts_quota_dimension;
ALTER TABLE accounts ADD CONSTRAINT chk_accounts_quota_dimension
    CHECK (quota_dimension IN ('global', 'spark', 'web_images', 'work_images', 'bps'));

CREATE UNIQUE INDEX IF NOT EXISTS accounts_bps_shadow_unique_idx
    ON accounts (parent_account_id)
    WHERE parent_account_id IS NOT NULL AND quota_dimension = 'bps' AND deleted_at IS NULL;

ALTER TABLE accounts ADD CONSTRAINT accounts_bps_shadow_check
    CHECK (quota_dimension <> 'bps' OR (parent_account_id IS NOT NULL AND concurrency > 0));
