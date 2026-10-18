//go:build unit

package repository

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthCASUsesExactSnapshotAndAtomicOutbox(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows int64
		err  error
	}{{"success", 1, nil}, {"conflict", 0, nil}, {"failure", 0, errors.New("db down")}} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &recordingSQLExecutor{result: rowsAffectedResult(tc.rows), err: tc.err}
			repo := newAccountRepositoryWithSQL(nil, exec, nil)
			applied, err := repo.UpdateOpenAIOAuthCredentialsIfUnchanged(context.Background(), 7, map[string]any{"refresh_token": "old"}, nil, map[string]any{"refresh_token": "new"})
			require.Equal(t, tc.rows == 1, applied)
			require.ErrorIs(t, err, tc.err)
			require.Len(t, exec.execQueries, 1)
			sql := normalizeSQLWhitespace(exec.execQueries[0])
			require.Contains(t, sql, "credentials = $3::jsonb")
			require.Contains(t, sql, "proxy_id IS NOT DISTINCT FROM $4")
			require.Contains(t, sql, "platform = 'openai' AND type = 'oauth'")
			require.Contains(t, sql, "parent_account_id IS NULL")
			require.Contains(t, sql, `SELECT $5, id, NULL, '{"cache_only":true}'::jsonb FROM updated`)
		})
	}
}

func TestOpenAIRefreshLeaseOldOwnerCannotReleaseSuccessor(t *testing.T) {
	redisServer := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cache := NewGeminiTokenCache(rdb).(service.OAuthRefreshLeaseCache)
	ctx := context.Background()
	first, err := cache.AcquireRefreshLease(ctx, "openai:account:7", time.Second)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	blocked, err := cache.AcquireRefreshLease(ctx, "openai:account:7", time.Second)
	require.NoError(t, err)
	require.Empty(t, blocked)
	redisServer.FastForward(2 * time.Second)
	second, err := cache.AcquireRefreshLease(ctx, "openai:account:7", time.Minute)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	require.NoError(t, cache.ReleaseRefreshLease(ctx, "openai:account:7", first))
	owns, err := cache.OwnsRefreshLease(ctx, "openai:account:7", second)
	require.NoError(t, err)
	require.True(t, owns)
	oldOwns, err := cache.OwnsRefreshLease(ctx, "openai:account:7", first)
	require.NoError(t, err)
	require.False(t, oldOwns)
}

func TestOpenAIMetadataUpdateOmitsCredentialWrite(t *testing.T) {
	var updateSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(_, actual string) error {
		if strings.HasPrefix(actual, "UPDATE") {
			updateSQL = actual
		}
		return nil
	})))
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	mock.ExpectQuery("lock").WillReturnRows(sqlmock.NewRows(openCodeGoMergeMockColumns()).AddRow(true, true, true, nil, nil, nil, nil, nil, nil, true, nil, nil))
	stopped := errors.New("stop before update execution")
	mock.ExpectBegin()
	mock.ExpectExec("update").WillReturnError(stopped)
	mock.ExpectRollback()
	repo := newAccountRepositoryWithSQL(client, db, nil)
	account := &service.Account{ID: 7, Name: "delivery", Concurrency: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "stale"}, Status: service.StatusActive}
	_, err = repo.updateLockedAccount(service.PreserveOpenAICredentialsUpdate(context.Background()), client, account, nil, nil, nil)
	require.ErrorIs(t, err, stopped)
	require.NoError(t, mock.ExpectationsWereMet())
	require.NotEmpty(t, updateSQL)
	require.NotContains(t, strings.Split(updateSQL, "RETURNING")[0], `"credentials" =`)
}
