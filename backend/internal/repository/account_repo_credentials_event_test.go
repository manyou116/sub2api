package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUpdateCredentialsKeepsDistinctDurableCacheOnlyEvent(t *testing.T) {
	for _, eventFails := range []bool{false, true} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
		t.Cleanup(func() { _ = client.Close() })
		repo := newAccountRepositoryWithSQL(client, db, nil)
		id := int64(71)
		payload := []byte(`{"cache_only":true}`)
		key := schedulerOutboxDedupKey(service.SchedulerOutboxEventAccountChanged, &id, nil, payload)
		require.NotEqual(t, schedulerOutboxDedupKey(service.SchedulerOutboxEventAccountChanged, &id, nil, nil), key,
			"credential refresh must not deduplicate a pending status or membership update")

		mock.ExpectBegin()
		mock.ExpectExec(`(?s)UPDATE accounts.*credentials = \$1::jsonb.*WHERE id = \$2 AND deleted_at IS NULL`).
			WithArgs(`{"access_token":"refreshed"}`, id).
			WillReturnResult(sqlmock.NewResult(0, 1))
		event := mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
			WithArgs(service.SchedulerOutboxEventAccountChanged, id, nil, payload, key)
		wantErr := errors.New("outbox unavailable")
		if eventFails {
			event.WillReturnError(wantErr)
			mock.ExpectRollback()
		} else {
			event.WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectCommit()
		}
		err = repo.UpdateCredentials(context.Background(), id, map[string]any{"access_token": "refreshed"})
		if eventFails {
			require.ErrorIs(t, err, wantErr)
		} else {
			require.NoError(t, err)
		}
		require.NoError(t, mock.ExpectationsWereMet())
	}
}

func TestUpdateGrokOAuthCredentialsCacheOnlyEventRequiresSuccessfulCAS(t *testing.T) {
	wantErr := errors.New("atomic credentials and outbox statement failed")
	for _, tc := range []struct {
		name string
		rows int64
		err  error
	}{
		{name: "compare_and_set_miss"},
		{name: "storage_failure", err: wantErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &recordingSQLExecutor{result: rowsAffectedResult(tc.rows), err: tc.err}
			repo := newAccountRepositoryWithSQL(nil, exec, nil)
			applied, err := repo.UpdateGrokOAuthCredentialsIfUnchanged(context.Background(), 71,
				map[string]any{"refresh_token": "expected"}, nil,
				map[string]any{"refresh_token": "rotated"})
			require.False(t, applied)
			require.ErrorIs(t, err, tc.err)
			require.Len(t, exec.execQueries, 1, "a failed CAS must not issue a separate outbox insertion")
			query := normalizeSQLWhitespace(exec.execQueries[0])
			require.Contains(t, query, "a.credentials = $5::jsonb")
			require.Contains(t, query, "a.proxy_id IS NOT DISTINCT FROM $6")
			require.Contains(t, query, `SELECT $7, updated.id, NULL, '{"cache_only":true}'::jsonb FROM updated`)
		})
	}
}
