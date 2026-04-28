-- Add group-level proxy fallback and OpenAI legacy image default settings.

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS openai_legacy_images_default BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS proxy_id BIGINT NULL;

CREATE INDEX IF NOT EXISTS idx_groups_proxy_id ON groups(proxy_id) WHERE proxy_id IS NOT NULL;