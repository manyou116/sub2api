//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestMiniMaxMigrationPreservesExistingKiroQuota(t *testing.T) {
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	// Temporary copies isolate constraint changes from the shared test schema.
	_, err = tx.ExecContext(ctx, `
		CREATE TEMP TABLE user_platform_quotas
			(LIKE public.user_platform_quotas INCLUDING ALL) ON COMMIT DROP;
		CREATE TEMP TABLE composite_model_routes
			(LIKE public.composite_model_routes INCLUDING ALL) ON COMMIT DROP;
		CREATE TEMP TABLE channel_monitors
			(LIKE public.channel_monitors INCLUDING ALL) ON COMMIT DROP;
		CREATE TEMP TABLE channel_monitor_request_templates
			(LIKE public.channel_monitor_request_templates INCLUDING ALL) ON COMMIT DROP;
	`)
	require.NoError(t, err)
	previous, err := migrations.FS.ReadFile("224_user_platform_quotas_add_cn_providers.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(previous))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO user_platform_quotas
		(user_id, platform, daily_limit_usd, daily_usage_usd)
		VALUES (1, 'kiro', 12, 3)`)
	require.NoError(t, err)

	upgrade, err := migrations.FS.ReadFile("237_add_minimax_platform.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(ctx, string(upgrade))
		require.NoError(t, err, "upgrade must preserve Kiro rows and be repeatable")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO user_platform_quotas
		(user_id, platform, daily_limit_usd) VALUES (1, 'minimax', 5)`)
	require.NoError(t, err)

	var limit, used float64
	err = tx.QueryRowContext(ctx, `SELECT daily_limit_usd, daily_usage_usd
		FROM user_platform_quotas WHERE user_id = 1 AND platform = 'kiro'`).Scan(&limit, &used)
	require.NoError(t, err)
	require.Equal(t, 12.0, limit)
	require.Equal(t, 3.0, used)
}
