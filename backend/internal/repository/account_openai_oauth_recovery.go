package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *accountRepository) TransitionOpenAIOAuthRecovery(ctx context.Context, a *service.Account, expected, next *service.OpenAIOAuthRecovery, credentials map[string]any) (bool, error) {
	if r.sql == nil || a == nil {
		return false, errors.New("OpenAI OAuth recovery repository unavailable")
	}
	oldCredentials, err := json.Marshal(normalizeJSONMap(a.Credentials))
	if err != nil {
		return false, err
	}
	oldState, err := json.Marshal(expected)
	if err != nil {
		return false, err
	}
	newState, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	var newCredentials any
	if credentials != nil {
		encoded, marshalErr := json.Marshal(normalizeJSONMap(credentials))
		if marshalErr != nil {
			return false, marshalErr
		}
		newCredentials = string(encoded)
	}
	needsReauth := next != nil && next.Phase == "reauth"
	result, err := r.sql.ExecContext(ctx, `WITH updated AS (
 UPDATE accounts SET
 extra = CASE WHEN $4::jsonb = 'null'::jsonb THEN COALESCE(extra, '{}'::jsonb) - 'openai_oauth_recovery'
 ELSE jsonb_set(COALESCE(extra, '{}'::jsonb), '{openai_oauth_recovery}', $4::jsonb) END,
 credentials = COALESCE($5::jsonb, credentials),
 status = CASE WHEN $6 THEN 'error' ELSE status END,
 error_message = CASE WHEN $6 THEN 'OpenAI OAuth credentials require reauthorization' ELSE error_message END,
 updated_at = NOW()
 WHERE id = $1 AND deleted_at IS NULL AND platform = 'openai' AND type = 'oauth'
 AND (status = 'active' OR ($5::jsonb IS NOT NULL AND $3::jsonb->>'phase' = 'refreshing'))
 AND credentials = $2::jsonb
 AND COALESCE(extra->'openai_oauth_recovery', 'null'::jsonb) = $3::jsonb
 AND proxy_id IS NOT DISTINCT FROM $7
 RETURNING id)
 INSERT INTO scheduler_outbox(event_type, account_id, group_id, payload)
 SELECT $8, id, NULL, '{"cache_only":true}'::jsonb FROM updated`, a.ID, string(oldCredentials), string(oldState), string(newState), newCredentials, needsReauth, a.ProxyID, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err == nil && n > 0 {
		r.syncSchedulerAccountSnapshotDetached(ctx, a.ID)
	}
	return n > 0, err
}

func (r *accountRepository) ListOpenAIOAuthRecoveryCandidates(ctx context.Context, afterID int64, limit int) ([]service.Account, error) {
	if r.sql == nil || limit < 1 || limit > 200 {
		return nil, errors.New("invalid OpenAI OAuth recovery page")
	}
	// Existing extra GIN index contains the recovery cohort; do not scan the pool.
	rows, err := r.sql.QueryContext(ctx, `SELECT id FROM accounts
 WHERE deleted_at IS NULL AND status = 'active' AND platform = 'openai' AND type = 'oauth' AND id > $1
 AND (extra @> '{"openai_oauth_recovery":{"phase":"pending"}}'::jsonb
 OR extra @> '{"openai_oauth_recovery":{"phase":"refreshing"}}'::jsonb
 OR extra @> '{"openai_oauth_recovery":{"phase":"probing"}}'::jsonb)
 ORDER BY id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	found, err := r.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]service.Account, 0, len(found))
	for _, a := range found {
		out = append(out, *a)
	}
	return out, nil
}
