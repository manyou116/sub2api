ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS image_concurrency integer NOT NULL DEFAULT 1;

UPDATE accounts
SET image_concurrency = 1
WHERE image_concurrency <= 0;