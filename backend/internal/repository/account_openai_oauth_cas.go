package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// UpdateOpenAIOAuthCredentialsIfUnchanged 对完整凭据及代理做 CAS，凭据和调度失效事件同一语句提交。
func (r *accountRepository) UpdateOpenAIOAuthCredentialsIfUnchanged(ctx context.Context, id int64, expected map[string]any, proxyID *int64, credentials map[string]any) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("account SQL executor unavailable")
	}
	oldJSON, err := json.Marshal(normalizeJSONMap(expected))
	if err != nil {
		return false, err
	}
	nextJSON, err := json.Marshal(normalizeJSONMap(credentials))
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `WITH updated AS (
 UPDATE accounts SET credentials = $1::jsonb, updated_at = NOW()
 WHERE id = $2 AND deleted_at IS NULL AND platform = 'openai' AND type = 'oauth'
 AND parent_account_id IS NULL AND credentials = $3::jsonb AND proxy_id IS NOT DISTINCT FROM $4
 RETURNING id)
 INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
 SELECT $5, id, NULL, '{"cache_only":true}'::jsonb FROM updated`,
		string(nextJSON), id, string(oldJSON), proxyID, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}
