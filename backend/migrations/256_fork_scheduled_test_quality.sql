ALTER TABLE scheduled_test_plans
    ADD COLUMN IF NOT EXISTS quality_prompt text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS quality_expected text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS quality_priority boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS quality_isolation_failures integer NOT NULL DEFAULT 0 CHECK (quality_isolation_failures BETWEEN 0 AND 8);

ALTER TABLE scheduled_test_results
    ADD COLUMN IF NOT EXISTS quality_verdict text NOT NULL DEFAULT '';

ALTER TABLE scheduled_test_plans ADD COLUMN IF NOT EXISTS quality_revision bigint NOT NULL DEFAULT 0;
ALTER TABLE scheduled_test_results ADD COLUMN IF NOT EXISTS quality_revision bigint NOT NULL DEFAULT 0;
