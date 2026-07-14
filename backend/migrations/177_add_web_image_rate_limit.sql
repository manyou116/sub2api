-- Web image (ChatGPT Web) rate-limit windows — durable across restarts / multi-instance.
-- Separate from text rate_limit_* so Web images can still bypass text 429 while honoring image cooldown.

ALTER TABLE accounts
	ADD COLUMN IF NOT EXISTS web_image_rate_limited_at TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS web_image_rate_limit_reset_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_accounts_web_image_rate_limit_reset_at
	ON accounts (web_image_rate_limit_reset_at)
	WHERE deleted_at IS NULL AND web_image_rate_limit_reset_at IS NOT NULL;

COMMENT ON COLUMN accounts.web_image_rate_limited_at IS 'ChatGPT Web image path: when image rate-limit/cooldown was armed';
COMMENT ON COLUMN accounts.web_image_rate_limit_reset_at IS 'ChatGPT Web image path: do not schedule web images until this time';
