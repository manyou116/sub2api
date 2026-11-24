//go:build unit

package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestForkOpenAIPersistence(t *testing.T) {
	dsn := os.Getenv("SUB2API_FORK_INTEGRATION_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL database not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, ApplyMigrations(ctx, db))
	require.NoError(t, ApplyMigrations(ctx, db))

	var accountID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO accounts (name, platform, type, credentials)
		VALUES ('connection test','openai','oauth','{"email":"operator@example.test","chatgpt_account_id":"workspace","access_token":"original"}')
		RETURNING id`).Scan(&accountID))
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", accountID) })
	accounts := &accountRepository{sql: db}
	for _, kind := range []string{"siwc", "excel"} {
		require.NoError(t, accounts.EnsureOpenAIConnection(ctx, accountID, kind, "host-"+kind))
		require.NoError(t, accounts.EnsureOpenAIConnection(ctx, accountID, kind, "stale"))
		require.NoError(t, accounts.SaveOpenAIConnection(ctx, accountID, kind, 0, "operator@example.test", "workspace", "sealed-"+kind))
		require.Error(t, accounts.SaveOpenAIConnection(ctx, accountID, kind, 0, "operator@example.test", "workspace", "stale-grant"))
		require.Error(t, accounts.SaveOpenAIConnection(ctx, accountID, kind, 1, "operator@example.test", "other-workspace", "wrong-grant"))
		record, err := accounts.ReadOpenAIConnection(ctx, accountID, kind)
		require.NoError(t, err)
		require.Equal(t, "host-"+kind, record.HostID)
		require.Equal(t, "sealed-"+kind, record.Ciphertext)
		require.EqualValues(t, 1, record.Revision)
	}
	var original string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT credentials->>'access_token' FROM accounts WHERE id = $1", accountID).Scan(&original))
	require.Equal(t, "original", original)

	plans := NewScheduledTestPlanRepository(db)
	results := NewScheduledTestResultRepository(db)
	now := time.Now()
	plan, err := plans.Create(ctx, &service.ScheduledTestPlan{
		AccountID: accountID, ModelID: "gpt-test", CronExpression: "*/15 * * * *", Enabled: true,
		NextRunAt: &now, MaxResults: 8, QualityPrompt: "Only answer 4", QualityExpected: "4", QualityPriority: true, QualityIsolationFailures: 1,
	})
	require.NoError(t, err)
	claimer := plans.(*scheduledTestPlanRepository)
	next := now.Add(time.Minute)
	claimed, err := claimer.ClaimQualityPlan(ctx, plan.ID, plan.QualityRevision, *plan.NextRunAt, next)
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = claimer.ClaimQualityPlan(ctx, plan.ID, plan.QualityRevision, *plan.NextRunAt, next)
	require.NoError(t, err)
	require.False(t, claimed)
	for _, verdict := range []string{"pass", "pass", "fail", ""} {
		_, err = results.Create(ctx, &service.ScheduledTestResult{
			PlanID: plan.ID, Status: "success", StartedAt: now, FinishedAt: now, QualityVerdict: verdict, QualityRevision: plan.QualityRevision,
		})
		require.NoError(t, err)
	}
	samples, err := accounts.RecentOpenAIQualitySamples(ctx, now.Add(-time.Hour))
	require.NoError(t, err)
	require.Contains(t, samples, service.OpenAIQualitySample{AccountID: accountID, Model: "gpt-test", Passed: 2, Total: 3, Isolated: true})
	_, err = results.Create(ctx, &service.ScheduledTestResult{
		PlanID: plan.ID, Status: "success", StartedAt: now, FinishedAt: now, QualityVerdict: "pass", QualityRevision: plan.QualityRevision,
	})
	require.NoError(t, err)
	samples, err = accounts.RecentOpenAIQualitySamples(ctx, now.Add(-time.Hour))
	require.NoError(t, err)
	require.Contains(t, samples, service.OpenAIQualitySample{AccountID: accountID, Model: "gpt-test", Passed: 3, Total: 4})
	plan.QualityExpected = "different"
	changed, err := plans.Update(ctx, plan)
	require.NoError(t, err)
	require.Equal(t, plan.QualityRevision+1, changed.QualityRevision)
	samples, err = accounts.RecentOpenAIQualitySamples(ctx, now.Add(-time.Hour))
	require.NoError(t, err)
	for _, sample := range samples {
		require.NotEqual(t, accountID, sample.AccountID)
	}
}
