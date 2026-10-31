package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *accountRepository) ExpireOpenAIOAuthToken(ctx context.Context, a *service.Account, token string, next *service.OpenAIOAuth401, until time.Time) (bool, error) {
	if r.sql == nil || a == nil || next == nil || token == "" {
		return false, errors.New("invalid OpenAI OAuth expiry update")
	}
	previous, err := json.Marshal(a.Extra[service.OpenAIOAuth401Key])
	if err != nil {
		return false, err
	}
	marker, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `WITH updated AS (
 UPDATE accounts SET
 credentials = jsonb_set(credentials, '{expires_at}', to_jsonb($4::text)),
 extra = jsonb_set(COALESCE(extra, '{}'::jsonb), '{openai_oauth_401}', $5::jsonb),
 temp_unschedulable_until = CASE WHEN temp_unschedulable_until > NOW() AND COALESCE(temp_unschedulable_reason, '') NOT LIKE 'OAuth 401:%' THEN temp_unschedulable_until ELSE $6 END,
 temp_unschedulable_reason = CASE WHEN temp_unschedulable_until > NOW() AND COALESCE(temp_unschedulable_reason, '') NOT LIKE 'OAuth 401:%' THEN temp_unschedulable_reason ELSE $7 END,
 status = CASE WHEN $8 THEN 'error' ELSE status END,
 error_message = CASE WHEN $8 THEN 'Repeated expired-token 401s after OAuth refresh; reauthorization required' ELSE error_message END,
 updated_at = NOW()
 WHERE id = $1 AND deleted_at IS NULL AND platform = 'openai' AND type = 'oauth' AND status = 'active'
 AND credentials->>'access_token' = $2 AND btrim(credentials->>'refresh_token') <> ''
 AND COALESCE(extra->'openai_oauth_401', 'null'::jsonb) = $3::jsonb
 RETURNING id)
 INSERT INTO scheduler_outbox(event_type, account_id, payload)
 SELECT $9, id, '{"cache_only":true}'::jsonb FROM updated`, a.ID, token, string(previous), time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), string(marker), until, service.OpenAIOAuth401Reason, next.Count >= 3, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err == nil && n > 0 {
		r.syncSchedulerAccountSnapshotDetached(ctx, a.ID)
	}
	return n > 0, err
}

func (r *accountRepository) ClearOpenAIOAuth401Pause(ctx context.Context, id int64, token string) (bool, error) {
	if r.sql == nil {
		return false, errors.New("OpenAI OAuth expiry repository unavailable")
	}
	result, err := r.sql.ExecContext(ctx, `WITH updated AS (
 UPDATE accounts SET temp_unschedulable_until = NULL, temp_unschedulable_reason = NULL, updated_at = NOW()
 WHERE id = $1 AND deleted_at IS NULL AND platform = 'openai' AND type = 'oauth' AND status = 'active'
 AND credentials->>'access_token' = $2 AND extra#>>'{openai_oauth_401,token_hash}' <> $3
 AND temp_unschedulable_reason = $4
 RETURNING id)
 INSERT INTO scheduler_outbox(event_type, account_id, payload)
 SELECT $5, id, '{"cache_only":true}'::jsonb FROM updated`, id, token, service.OpenAIOAuthTokenHash(token), service.OpenAIOAuth401Reason, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err == nil && n > 0 {
		r.syncSchedulerAccountSnapshotDetached(ctx, id)
	}
	return n > 0, err
}
