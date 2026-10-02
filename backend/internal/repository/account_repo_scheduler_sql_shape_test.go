package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Capture the actual Ent SQL: a wide DISTINCT would add unnecessary sorting or
// hashing of credentials and extra when a bucket contains tens of thousands of
// accounts. Account IDs and the account-group primary key are already unique.
func TestSchedulerGroupQuerySQLAvoidsWideDistinctAndPreservesOrder(t *testing.T) {
	var queries []string
	matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		queries = append(queries, normalizeSQLWhitespace(actual))
		return sqlmock.QueryMatcherRegexp.Match(expected, actual)
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)
	now := time.Now()
	groupRows := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"account_id", "group_id", "priority", "created_at"}).
			AddRow(int64(29), int64(7), 1, now).
			AddRow(int64(11), int64(7), 2, now)
	}
	mock.ExpectQuery(`FROM "account_groups"`).WillReturnRows(groupRows())
	// The hydration query may return a different order from the ordered edges.
	mock.ExpectQuery(`FROM "accounts"`).WillReturnRows(
		sqlmock.NewRows([]string{"id", "platform", "type", "status", "schedulable"}).
			AddRow(int64(11), service.PlatformOpenAI, service.AccountTypeOAuth, service.StatusActive, true).
			AddRow(int64(29), service.PlatformOpenAI, service.AccountTypeOAuth, service.StatusActive, true))
	mock.ExpectQuery(`FROM "account_groups"`).WillReturnRows(groupRows())
	mock.ExpectQuery(`FROM "groups"`).WillReturnRows(
		sqlmock.NewRows([]string{"id", "name", "platform", "status"}).
			AddRow(int64(7), "scheduler-sql-test", service.PlatformOpenAI, service.StatusActive))
	mock.ExpectQuery(`FROM accounts`).WillReturnRows(
		sqlmock.NewRows([]string{"id", "web_image_rate_limited_at", "web_image_rate_limit_reset_at"}))

	accounts, err := repo.ListSchedulableByGroupIDAndPlatform(context.Background(), 7, service.PlatformOpenAI)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, queries, 5)
	require.Len(t, accounts, 2)
	require.Equal(t, int64(29), accounts[0].ID)
	require.Equal(t, int64(11), accounts[1].ID)
	require.Equal(t, []int64{7}, accounts[0].GroupIDs)
	for i, query := range queries {
		require.NotContains(t, strings.ToUpper(query), "SELECT DISTINCT", "query %d: %s", i, query)
	}
	order := queries[0][strings.Index(queries[0], " ORDER BY "):]
	require.Contains(t, order, `"account_groups"."priority"`)
	require.Contains(t, queries[0], `"accounts"."priority"`)
	for _, column := range []string{"deleted_at", "status", "platform", "schedulable", "temp_unschedulable_until", "expires_at", "auto_pause_on_expired", "overload_until", "rate_limit_reset_at"} {
		require.Contains(t, queries[0], column)
	}
	require.Contains(t, queries[1], `"credentials"`)
	require.Contains(t, queries[1], `"extra"`)
	require.NotContains(t, queries[1], " ORDER BY ")
	t.Logf("membership SQL: %s", queries[0])
	t.Logf("hydration SQL: %s", queries[1])
}
