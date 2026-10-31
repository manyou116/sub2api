package repository

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthRecoveryAtomicStateAndCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result int64
		err    error
	}{{"applied", 1, nil}, {"stale", 0, nil}, {"outbox failure", 0, errors.New("outbox unavailable")}} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &recordingSQLExecutor{result: rowsAffectedResult(tc.result), err: tc.err}
			repo := newAccountRepositoryWithSQL(nil, exec, nil)
			state := &service.OpenAIOAuthRecovery{Phase: "pending", Generation: "g", TokenHash: "hash"}
			a := &service.Account{ID: 41, Credentials: map[string]any{"access_token": "old", "refresh_token": "rt"}}
			applied, err := repo.TransitionOpenAIOAuthRecovery(context.Background(), a, nil, state, nil)
			require.Equal(t, tc.result > 0, applied)
			if tc.err != nil {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, exec.execQueries, 1)
			q := normalizeSQLWhitespace(exec.execQueries[0])
			require.Contains(t, q, "credentials = $2::jsonb")
			require.Contains(t, q, "proxy_id IS NOT DISTINCT FROM $7")
			require.Contains(t, q, "COALESCE(extra->'openai_oauth_recovery', 'null'::jsonb) = $3::jsonb")
			require.Contains(t, q, "INSERT INTO scheduler_outbox")
			require.NotContains(t, q, "schedulable =")
			require.NotContains(t, q, "temp_unschedulable_until =")
			require.Contains(t, q, "AND (status = 'active' OR ($5::jsonb IS NOT NULL AND $3::jsonb->>'phase' = 'refreshing'))")
			require.Equal(t, "null", exec.execArgs[0][2])
			require.Nil(t, exec.execArgs[0][4])
			require.Equal(t, false, exec.execArgs[0][5])
			require.False(t, strings.Contains(q, "refresh_token ="))
		})
	}
}
