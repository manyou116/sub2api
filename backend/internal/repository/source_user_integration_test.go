//go:build integration

package repository

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSourceUserDatabaseAttributionAndStatistics(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := newUsageLogRepositoryWithSQL(client, integrationDB)
	user := mustCreateUser(t, client, &service.User{Email: uuid.NewString() + "@source.test"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: uuid.NewString(), Name: "source-test"})
	account := mustCreateAccount(t, client, &service.Account{Name: "source-test"})
	// This test exercises committed batch writes. Keep global dashboard suites
	// independent by removing only this test's rows after all assertions finish.
	t.Cleanup(func() {
		for _, cleanup := range []struct {
			query string
			id    int64
		}{
			{"DELETE FROM ops_error_logs WHERE api_key_id = $1", key.ID},
			{"DELETE FROM usage_logs WHERE api_key_id = $1", key.ID},
			{"DELETE FROM api_keys WHERE id = $1", key.ID},
			{"DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID},
			{"DELETE FROM accounts WHERE id = $1", account.ID},
			{"DELETE FROM users WHERE id = $1", user.ID},
		} {
			_, err := integrationDB.ExecContext(ctx, cleanup.query, cleanup.id)
			require.NoError(t, err)
		}
	})
	start, end := time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour)
	identities := []*service.SourceUser{nil,
		{SourceID: "factory", UserID: "123", Username: "old-name"},
		{SourceID: "factory", UserID: "123", Username: "new-name"},
		{SourceID: "other", UserID: "123", Username: "other-site"},
	}
	for i, identity := range identities {
		log := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, RequestID: uuid.NewString(), Model: "gpt-test", InputTokens: 10, OutputTokens: 5, ActualCost: 1, TotalCost: 2, CreatedAt: time.Now().UTC(), UpstreamRequestBody: &service.UpstreamRequestBodyStats{RawBytes: 1024, EncodedBytes: 256}}
		requestCtx := service.WithSourceUser(ctx, identity)
		if i%2 == 0 {
			_, err := repo.Create(requestCtx, log)
			require.NoError(t, err)
		} else {
			require.NoError(t, repo.CreateBestEffort(requestCtx, log))
		}
		var id int64
		require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT id FROM usage_logs WHERE request_id=$1 AND api_key_id=$2", log.RequestID, key.ID).Scan(&id))
		got, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		require.Equal(t, identity, got.SourceUser)
	}
	filters := UsageLogFilters{APIKeyID: key.ID, SourceID: "factory", SourceUserID: "123", StartTime: &start, EndTime: &end}
	logs, page, err := repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 20}, filters)
	require.NoError(t, err)
	require.Len(t, logs, 2)
	require.EqualValues(t, 2, page.Total)
	stats, err := repo.GetStatsWithFilters(ctx, filters)
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.TotalRequests)
	require.EqualValues(t, 30, stats.TotalTokens)
	require.Equal(t, 2.0, stats.TotalActualCost)
	require.EqualValues(t, 512, stats.UpstreamBody.EncodedBytes)
	require.EqualValues(t, 2, stats.UpstreamBody.MeasuredRequests)
	models, err := repo.GetModelStatsWithUsageFiltersBySource(ctx, start, end, filters, "requested")
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.EqualValues(t, 2, models[0].Requests)
	trend, err := repo.GetUsageTrendWithUsageFilters(ctx, start, end, "day", filters)
	require.NoError(t, err)
	var requests int64
	for _, point := range trend {
		requests += point.Requests
	}
	require.EqualValues(t, 2, requests)
	groups, err := repo.GetGroupStatsWithUsageFilters(ctx, start, end, filters)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.EqualValues(t, 2, groups[0].Requests)
	ops := NewOpsRepository(integrationDB)
	for _, identity := range identities[1:] {
		id, err := ops.InsertErrorLog(ctx, &service.OpsInsertErrorLogInput{SourceUser: identity, UserID: &user.ID, APIKeyID: &key.ID, RequestID: uuid.NewString(), ErrorPhase: "upstream", ErrorType: "upstream_error", Severity: "error", StatusCode: 400, CreatedAt: time.Now().UTC()})
		require.NoError(t, err)
		got, err := ops.GetErrorLogByID(ctx, id)
		require.NoError(t, err)
		require.Equal(t, identity, got.SourceUser)
	}
	errors, err := ops.ListErrorLogs(ctx, &service.OpsErrorLogFilter{APIKeyID: &key.ID, SourceID: "factory", SourceUserID: "123", Page: 1, PageSize: 20})
	require.NoError(t, err)
	require.Len(t, errors.Errors, 2)
	// Additive migration reruns must preserve the snapshots and existing rows.
	for range 2 {
		data, err := os.ReadFile("../../migrations/250_source_user_attribution.sql")
		require.NoError(t, err)
		tx, err := integrationDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, string(data))
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		data, err = os.ReadFile("../../migrations/251_source_user_indexes_notx.sql")
		require.NoError(t, err)
		// CONCURRENTLY statements are separate protocol commands, outside a transaction.
		for _, statement := range strings.Split(string(data), ";") {
			if strings.TrimSpace(statement) != "" {
				_, err = integrationDB.ExecContext(ctx, statement)
				require.NoError(t, err)
			}
		}
	}
	stats, err = repo.GetStatsWithFilters(ctx, filters)
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.TotalRequests)
}

func TestSourceUserMigrationPreservesHistoricalRows(t *testing.T) {
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(ctx, `
CREATE TEMP TABLE usage_logs (LIKE public.usage_logs INCLUDING DEFAULTS INCLUDING CONSTRAINTS) ON COMMIT DROP;
CREATE TEMP TABLE ops_error_logs (LIKE public.ops_error_logs INCLUDING DEFAULTS INCLUDING CONSTRAINTS) ON COMMIT DROP;
ALTER TABLE usage_logs DROP COLUMN source_user;
ALTER TABLE ops_error_logs DROP COLUMN source_user;
INSERT INTO usage_logs (user_id,api_key_id,account_id,request_id,model,input_tokens,actual_cost)
VALUES (1,1,1,'historical-request','gpt-test',100,0.5);
INSERT INTO ops_error_logs (error_phase,error_type,severity,status_code) VALUES ('upstream','upstream_error','error',400);
`)
	require.NoError(t, err)
	data, err := os.ReadFile("../../migrations/250_source_user_attribution.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(ctx, string(data))
		require.NoError(t, err)
	}
	var tokens int
	var cost float64
	var unknown bool
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT input_tokens,actual_cost,source_user IS NULL FROM usage_logs WHERE request_id='historical-request'").Scan(&tokens, &cost, &unknown))
	require.Equal(t, 100, tokens)
	require.Equal(t, 0.5, cost)
	require.True(t, unknown)
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT source_user IS NULL FROM ops_error_logs").Scan(&unknown))
	require.True(t, unknown)
}
