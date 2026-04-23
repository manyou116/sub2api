-- Add per-group default for ChatGPT Web legacy image generation routing.
-- openai_legacy_images_default: 当 OpenAI 分组下的 OAuth 账号未在 extra
-- 显式设置 openai_oauth_legacy_images 时使用此默认值。
-- 账号 extra.openai_oauth_legacy_images 显式 true/false 仍优先生效。
ALTER TABLE groups ADD COLUMN IF NOT EXISTS openai_legacy_images_default boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN groups.openai_legacy_images_default IS 'OpenAI 分组默认启用 ChatGPT Web 旧版生图链路（账号 extra.openai_oauth_legacy_images 显式 true/false 可覆盖）。';
