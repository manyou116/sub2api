package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const proxyAccountSummariesQuery = `
	SELECT id, name, platform, type, notes, proxy_id, status, schedulable
	FROM accounts
	WHERE proxy_id = $1 AND deleted_at IS NULL
	ORDER BY id DESC
`

func TestProxyAccountSummariesScopedState(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	// 精确限定列与代理条件，防止摘要退化成全账号、凭据或分页查询。
	mock.ExpectQuery(proxyAccountSummariesQuery).WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "platform", "type", "notes", "proxy_id", "status", "schedulable"}).
			AddRow(18, "paused", "openai", "oauth", "keep note", 9, "active", false).
			AddRow(17, "enabled", "anthropic", "apikey", nil, 9, "error", true)).
		RowsWillBeClosed()

	got, err := newProxyRepositoryWithSQL(nil, db).ListAccountSummariesByProxyID(context.Background(), 9)

	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(18), got[0].ID)
	require.Equal(t, "keep note", *got[0].Notes)
	require.Equal(t, int64(9), got[0].ProxyID)
	require.Equal(t, "active", got[0].Status)
	require.False(t, got[0].Schedulable)
	require.Nil(t, got[1].Notes)
	require.Equal(t, int64(9), got[1].ProxyID)
	require.Equal(t, "error", got[1].Status)
	require.True(t, got[1].Schedulable)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestProxyAccountSummariesEmpty(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectQuery(proxyAccountSummariesQuery).WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "platform", "type", "notes", "proxy_id", "status", "schedulable"})).
		RowsWillBeClosed()

	got, err := newProxyRepositoryWithSQL(nil, db).ListAccountSummariesByProxyID(context.Background(), 9)

	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestProxyAccountSummariesReadErrors(t *testing.T) {
	readErr := errors.New("account read failed")
	for _, stage := range []string{"query", "scan", "iteration"} {
		t.Run(stage, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			expect := mock.ExpectQuery(proxyAccountSummariesQuery).WithArgs(int64(9))
			rows := sqlmock.NewRows([]string{"id", "name", "platform", "type", "notes", "proxy_id", "status", "schedulable"})
			switch stage {
			case "query":
				expect.WillReturnError(readErr)
			case "scan":
				expect.WillReturnRows(rows.AddRow(18, "paused", "openai", "oauth", nil, 9, "active", "invalid bool")).RowsWillBeClosed()
			case "iteration":
				expect.WillReturnRows(rows.AddRow(18, "paused", "openai", "oauth", nil, 9, "active", false).RowError(0, readErr)).RowsWillBeClosed()
			}

			got, err := newProxyRepositoryWithSQL(nil, db).ListAccountSummariesByProxyID(context.Background(), 9)

			require.Error(t, err)
			if stage != "scan" {
				require.ErrorIs(t, err, readErr)
			}
			require.Nil(t, got)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
