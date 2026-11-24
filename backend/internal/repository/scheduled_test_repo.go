package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// --- Plan Repository ---

type scheduledTestPlanRepository struct {
	db *sql.DB
}

func NewScheduledTestPlanRepository(db *sql.DB) service.ScheduledTestPlanRepository {
	return &scheduledTestPlanRepository{db: db}
}

func (r *scheduledTestPlanRepository) Create(ctx context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO scheduled_test_plans (account_id, model_id, cron_expression, enabled, max_results, auto_recover, next_run_at, created_at, updated_at, quality_prompt, quality_expected, quality_priority, quality_isolation_failures)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW(), $8, $9, $10, $11)
		RETURNING id, account_id, model_id, cron_expression, enabled, max_results, auto_recover, last_run_at, next_run_at, created_at, updated_at, quality_prompt, quality_expected, quality_priority, quality_isolation_failures, quality_revision
	`, plan.AccountID, plan.ModelID, plan.CronExpression, plan.Enabled, plan.MaxResults, plan.AutoRecover, plan.NextRunAt, plan.QualityPrompt, plan.QualityExpected, plan.QualityPriority, plan.QualityIsolationFailures)
	return scanPlan(row)
}

func (r *scheduledTestPlanRepository) GetByID(ctx context.Context, id int64) (*service.ScheduledTestPlan, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, account_id, model_id, cron_expression, enabled, max_results, auto_recover, last_run_at, next_run_at, created_at, updated_at, quality_prompt, quality_expected, quality_priority, quality_isolation_failures, quality_revision
		FROM scheduled_test_plans WHERE id = $1
	`, id)
	return scanPlan(row)
}

func (r *scheduledTestPlanRepository) ListByAccountID(ctx context.Context, accountID int64) ([]*service.ScheduledTestPlan, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, account_id, model_id, cron_expression, enabled, max_results, auto_recover, last_run_at, next_run_at, created_at, updated_at, quality_prompt, quality_expected, quality_priority, quality_isolation_failures, quality_revision
		FROM scheduled_test_plans WHERE account_id = $1
		ORDER BY created_at DESC
	`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPlans(rows)
}

func (r *scheduledTestPlanRepository) ListDue(ctx context.Context, now time.Time) ([]*service.ScheduledTestPlan, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, account_id, model_id, cron_expression, enabled, max_results, auto_recover, last_run_at, next_run_at, created_at, updated_at, quality_prompt, quality_expected, quality_priority, quality_isolation_failures, quality_revision
		FROM scheduled_test_plans
		WHERE enabled = true AND next_run_at <= $1
		ORDER BY next_run_at ASC
	`, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPlans(rows)
}

func (r *scheduledTestPlanRepository) Update(ctx context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	row := r.db.QueryRowContext(ctx, `
		UPDATE scheduled_test_plans
		SET model_id = $2, cron_expression = $3, enabled = $4, max_results = $5, auto_recover = $6, next_run_at = $7, updated_at = NOW(), quality_prompt = $8, quality_expected = $9, quality_priority = $10, quality_isolation_failures = $11,
		quality_revision = CASE WHEN (model_id, enabled, quality_prompt, quality_expected, quality_priority, quality_isolation_failures)
		IS DISTINCT FROM ($2::varchar, $4::boolean, $8::text, $9::text, $10::boolean, $11::integer) THEN quality_revision + 1 ELSE quality_revision END
		WHERE id = $1
		RETURNING id, account_id, model_id, cron_expression, enabled, max_results, auto_recover, last_run_at, next_run_at, created_at, updated_at, quality_prompt, quality_expected, quality_priority, quality_isolation_failures, quality_revision
	`, plan.ID, plan.ModelID, plan.CronExpression, plan.Enabled, plan.MaxResults, plan.AutoRecover, plan.NextRunAt, plan.QualityPrompt, plan.QualityExpected, plan.QualityPriority, plan.QualityIsolationFailures)
	return scanPlan(row)
}

func (r *scheduledTestPlanRepository) Delete(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id = $1`, id)
	return err
}

func (r *scheduledTestPlanRepository) UpdateAfterRun(ctx context.Context, id int64, lastRunAt time.Time, nextRunAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE scheduled_test_plans SET last_run_at = $2, next_run_at = $3, updated_at = NOW() WHERE id = $1
	`, id, lastRunAt, nextRunAt)
	return err
}

// --- Result Repository ---

type scheduledTestResultRepository struct {
	db *sql.DB
}

func NewScheduledTestResultRepository(db *sql.DB) service.ScheduledTestResultRepository {
	return &scheduledTestResultRepository{db: db}
}

func (r *scheduledTestResultRepository) Create(ctx context.Context, result *service.ScheduledTestResult) (*service.ScheduledTestResult, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO scheduled_test_results (plan_id, status, response_text, error_message, latency_ms, started_at, finished_at, created_at, quality_verdict, quality_revision)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), $8, $9)
		RETURNING id, plan_id, status, response_text, error_message, latency_ms, started_at, finished_at, created_at, quality_verdict, quality_revision
	`, result.PlanID, result.Status, result.ResponseText, result.ErrorMessage, result.LatencyMs, result.StartedAt, result.FinishedAt, result.QualityVerdict, result.QualityRevision)

	out := &service.ScheduledTestResult{}
	if err := row.Scan(
		&out.ID, &out.PlanID, &out.Status, &out.ResponseText, &out.ErrorMessage,
		&out.LatencyMs, &out.StartedAt, &out.FinishedAt, &out.CreatedAt, &out.QualityVerdict, &out.QualityRevision,
	); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *scheduledTestResultRepository) ListByPlanID(ctx context.Context, planID int64, limit int) ([]*service.ScheduledTestResult, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, plan_id, status, response_text, error_message, latency_ms, started_at, finished_at, created_at, quality_verdict, quality_revision
		FROM scheduled_test_results
		WHERE plan_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, planID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []*service.ScheduledTestResult
	for rows.Next() {
		r := &service.ScheduledTestResult{}
		if err := rows.Scan(
			&r.ID, &r.PlanID, &r.Status, &r.ResponseText, &r.ErrorMessage,
			&r.LatencyMs, &r.StartedAt, &r.FinishedAt, &r.CreatedAt, &r.QualityVerdict, &r.QualityRevision,
		); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

func (r *scheduledTestResultRepository) PruneOldResults(ctx context.Context, planID int64, keepCount int) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM scheduled_test_results
		WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY plan_id ORDER BY created_at DESC) AS rn
				FROM scheduled_test_results
				WHERE plan_id = $1
			) ranked
			WHERE rn > $2
		)
	`, planID, keepCount)
	return err
}

// --- scan helpers ---

type scannable interface {
	Scan(dest ...any) error
}

func scanPlan(row scannable) (*service.ScheduledTestPlan, error) {
	p := &service.ScheduledTestPlan{}
	if err := row.Scan(
		&p.ID, &p.AccountID, &p.ModelID, &p.CronExpression, &p.Enabled, &p.MaxResults, &p.AutoRecover,
		&p.LastRunAt, &p.NextRunAt, &p.CreatedAt, &p.UpdatedAt, &p.QualityPrompt, &p.QualityExpected, &p.QualityPriority, &p.QualityIsolationFailures, &p.QualityRevision,
	); err != nil {
		return nil, err
	}
	return p, nil
}

func scanPlans(rows *sql.Rows) ([]*service.ScheduledTestPlan, error) {
	var plans []*service.ScheduledTestPlan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, rows.Err()
}

// ClaimQualityPlan advances one scheduled slot before dispatch. Concurrent
// replicas only run the plan whose exact due time and revision they observed.
func (r *scheduledTestPlanRepository) ClaimQualityPlan(ctx context.Context, id, revision int64, due, next time.Time) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE scheduled_test_plans SET next_run_at = $4
		WHERE id = $1 AND quality_revision = $2 AND next_run_at = $3 AND enabled`, id, revision, due, next)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (r *scheduledTestPlanRepository) FinishQualityPlan(ctx context.Context, id, revision int64, next, finished time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE scheduled_test_plans SET last_run_at = $4
		WHERE id = $1 AND quality_revision = $2 AND next_run_at = $3 AND enabled`, id, revision, next, finished)
	return err
}
