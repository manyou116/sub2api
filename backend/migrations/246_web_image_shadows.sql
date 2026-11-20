-- Extend linked credential accounts without modifying published Spark/Web migrations.
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS chk_accounts_quota_dimension;
ALTER TABLE accounts ADD CONSTRAINT chk_accounts_quota_dimension
    CHECK (quota_dimension IN ('global', 'spark', 'web_images'));

CREATE UNIQUE INDEX IF NOT EXISTS uq_accounts_web_image_shadow_per_parent
    ON accounts (parent_account_id)
    WHERE parent_account_id IS NOT NULL AND quota_dimension = 'web_images' AND deleted_at IS NULL;

ALTER TABLE accounts ADD CONSTRAINT chk_accounts_web_image_shadow_concurrency
    CHECK (quota_dimension <> 'web_images' OR concurrency > 0);
