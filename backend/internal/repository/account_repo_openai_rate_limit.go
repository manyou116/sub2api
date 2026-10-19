package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const openAIReconcileEligibleSQL = `deleted_at IS NULL AND platform = 'openai' AND type = 'oauth'
	AND parent_account_id IS NULL AND status = 'active' AND schedulable = TRUE
	AND extra @> '{"openai_rate_limit_reconcile_enabled":true}'::jsonb
	AND rate_limited_at IS NOT NULL AND rate_limit_reset_at > NOW()
	AND (expires_at IS NULL OR expires_at > NOW() OR auto_pause_on_expired IS NOT TRUE)
	AND (overload_until IS NULL OR overload_until <= NOW())
	AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until <= NOW())`

// Bounded claims move the due time before network I/O. Ordering by due time
// prevents low IDs that remain limited from starving the rest of a large pool.
func (r *accountRepository) ClaimOpenAIRateLimitReconcileAccounts(ctx context.Context, now time.Time, limit int, lease string) ([]*service.Account, error) {
	if limit <= 0 || lease == "" {
		return nil, nil
	}
	limit = min(limit, 20)
	rows, err := clientFromContext(ctx, r.client).QueryContext(ctx, `
		WITH due AS (
			SELECT id FROM accounts WHERE `+openAIReconcileEligibleSQL+`
			AND CASE WHEN (extra #>> '{openai_rate_limit_reconcile_state,next_check_unix}') ~ '^[0-9]{1,12}$'
				THEN (extra #>> '{openai_rate_limit_reconcile_state,next_check_unix}')::bigint ELSE 0 END <= $1
			ORDER BY CASE WHEN (extra #>> '{openai_rate_limit_reconcile_state,next_check_unix}') ~ '^[0-9]{1,12}$'
				THEN (extra #>> '{openai_rate_limit_reconcile_state,next_check_unix}')::bigint ELSE 0 END, id
			LIMIT $2 FOR UPDATE SKIP LOCKED
		)
		UPDATE accounts a SET extra = jsonb_set(a.extra, '{openai_rate_limit_reconcile_state}',
			(CASE WHEN jsonb_typeof(a.extra -> 'openai_rate_limit_reconcile_state') = 'object'
				THEN a.extra -> 'openai_rate_limit_reconcile_state' ELSE '{}'::jsonb END)
			|| jsonb_build_object('lease', $3::text, 'next_check_unix', $1::bigint + 600 + a.id % 60), true)
		FROM due WHERE a.id = due.id RETURNING a.id`, now.Unix(), limit, lease)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close() // Release the connection before hydrating bounded results.
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return r.GetByIDs(ctx, ids)
}

// Usage, cooldown and durable scheduler notification commit together. A new
// 429, opt-out, re-claim or credential/proxy replacement rejects the old probe.
func (r *accountRepository) CompleteOpenAIRateLimitReconcile(ctx context.Context, observed *service.Account, updates map[string]any, recovered bool) (bool, error) {
	if observed == nil || observed.RateLimitedAt == nil || observed.RateLimitResetAt == nil {
		return false, nil
	}
	payload, err := json.Marshal(updates)
	if err != nil {
		return false, err
	}
	credentials, err := json.Marshal(normalizeJSONMap(observed.Credentials))
	if err != nil {
		return false, err
	}
	state, err := json.Marshal(observed.Extra[service.OpenAIRateLimitReconcileStateExtraKey])
	if err != nil {
		return false, err
	}
	baseCtx := ctx
	contextTx := dbent.TxFromContext(ctx)
	client := clientFromContext(ctx, r.client)
	var tx *dbent.Tx
	if contextTx == nil {
		tx, err = r.client.Tx(ctx)
		if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
			return false, err
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			client = tx.Client()
		}
	}
	result, err := client.ExecContext(ctx, `UPDATE accounts SET
		extra = extra || $2::jsonb, updated_at = NOW(),
		rate_limited_at = CASE WHEN $3 THEN NULL ELSE rate_limited_at END,
		rate_limit_reset_at = CASE WHEN $3 THEN NULL ELSE rate_limit_reset_at END
		WHERE id = $1 AND `+openAIReconcileEligibleSQL+`
		AND rate_limited_at = $4 AND rate_limit_reset_at = $5
		AND credentials = $6::jsonb AND proxy_id IS NOT DISTINCT FROM $7
		AND extra -> 'openai_rate_limit_reconcile_state' = $8::jsonb
		AND COALESCE(extra #>> '{openai_rate_limit_reconcile_state,lease}', '') <> ''`,
		observed.ID, string(payload), recovered, *observed.RateLimitedAt, *observed.RateLimitResetAt,
		string(credentials), observed.ProxyID, string(state))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	if recovered {
		if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &observed.ID, nil, nil); err != nil {
			return false, err
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return false, err
		}
	}
	if contextTx == nil {
		r.syncSchedulerAccountSnapshot(baseCtx, observed.ID)
	}
	return true, nil
}

// Ordinary full-account edits preserve the latest opt-in and worker lease from
// the row already locked by updateLockedAccount. Use bulk/partial-extra updates
// to change the opt-in, so a stale edit form cannot undo a concurrent toggle.
func preserveOpenAIReconcileExtra(ctx context.Context, client *dbent.Client, account *service.Account, extra map[string]any) error {
	rows, err := client.QueryContext(ctx, `SELECT COALESCE(extra, '{}'::jsonb), rate_limited_at, rate_limit_reset_at
		FROM accounts WHERE id = $1`, account.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return service.ErrAccountNotFound
	}
	var raw []byte
	var limitedAt, resetAt sql.NullTime
	if err := rows.Scan(&raw, &limitedAt, &resetAt); err != nil {
		return err
	}
	var current map[string]any
	if err := json.Unmarshal(raw, &current); err != nil {
		return err
	}
	for _, key := range []string{service.OpenAIRateLimitReconcileEnabledExtraKey, service.OpenAIRateLimitReconcileStateExtraKey} {
		delete(extra, key)
		if value, ok := current[key]; ok {
			extra[key] = value
		}
	}
	if current[service.OpenAIRateLimitReconcileEnabledExtraKey] == true {
		// A stale ordinary edit must not resurrect a cooldown or usage snapshot
		// that reconciliation already cleared under this same row lock.
		account.RateLimitedAt, account.RateLimitResetAt = nil, nil
		if limitedAt.Valid {
			account.RateLimitedAt = &limitedAt.Time
		}
		if resetAt.Valid {
			account.RateLimitResetAt = &resetAt.Time
		}
		for key, value := range current {
			if key == "codex_usage_updated_at" || strings.HasPrefix(key, "codex_primary_") || strings.HasPrefix(key, "codex_secondary_") || strings.HasPrefix(key, "codex_5h_") || strings.HasPrefix(key, "codex_7d_") {
				extra[key] = value
			}
		}
	}
	return rows.Err()
}
