-- Add Kiro to the user_platform_quotas.platform CHECK constraint.
--
-- Kiro is a fork-owned platform (P5). Once it is exposed in
-- service.AllowedQuotaPlatforms and the admin/user quota UI, existing
-- databases must accept Kiro quota rows.
ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kiro'));
