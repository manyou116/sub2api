-- Add per-group 24h rolling quota for ChatGPT Web legacy image generation.
-- ChatGPT Web 实测每个 OAuth 账号约可在 24h 内生成 3 张图，超出则上游 429。
-- 本字段允许按分组覆写：0 = 不限（让 ChatGPT 自己 429 时由 circuit breaker 兜底）。
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS openai_legacy_images_daily_quota integer NOT NULL DEFAULT 3;

COMMENT ON COLUMN groups.openai_legacy_images_daily_quota IS
    'OpenAI 旧版生图每个账号 24 小时滚动配额（0 = 不限制；ChatGPT Web 实测约 3 张/24h）。';
