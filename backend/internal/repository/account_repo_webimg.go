package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// Fork-owned account repository extensions for ChatGPT Web image durable cooldown
// and capacity inventory that includes text-rate-limited webimg-capable accounts.
// Keep bulk logic here so upstream account_repo.go merges stay small.

func (r *accountRepository) ListSchedulableCapacityByGroupIDs(ctx context.Context, groupIDs []int64) ([]service.GroupAccountCapacityRow, error) {
	groupIDs = uniquePositiveInt64s(groupIDs)
	if len(groupIDs) == 0 {
		return []service.GroupAccountCapacityRow{}, nil
	}
	if r.sql == nil {
		rows := make([]service.GroupAccountCapacityRow, 0)
		for _, groupID := range groupIDs {
			accounts, err := r.ListSchedulableByGroupID(ctx, groupID)
			if err != nil {
				return nil, err
			}
			for i := range accounts {
				acc := &accounts[i]
				rows = append(rows, service.GroupAccountCapacityRow{
					GroupID:                  groupID,
					AccountID:                acc.ID,
					Concurrency:              acc.Concurrency,
					Extra:                    copyJSONMap(acc.Extra),
					SessionWindowStart:       acc.SessionWindowStart,
					SessionWindowEnd:         acc.SessionWindowEnd,
					SessionWindowStatus:      acc.SessionWindowStatus,
					RateLimitResetAt:         acc.RateLimitResetAt,
					OverloadUntil:            acc.OverloadUntil,
					WebImageRateLimitResetAt: acc.WebImageRateLimitResetAt,
					Platform:                 acc.Platform,
					Type:                     acc.Type,
				})
			}
		}
		return rows, nil
	}

	// Capacity inventory includes text-rate-limited accounts so web-image max/used
	// still reflects OAuth numbers that remain usable for ChatGPT Web images.
	rows, err := r.sql.QueryContext(ctx, `
		SELECT
			ag.group_id,
			a.id AS account_id,
			a.concurrency,
			COALESCE(a.extra, '{}'::jsonb)::text AS extra,
			a.session_window_start,
			a.session_window_end,
			COALESCE(a.session_window_status, '') AS session_window_status,
			a.rate_limit_reset_at,
			a.overload_until,
			a.web_image_rate_limit_reset_at,
			a.platform,
			a.type
		FROM account_groups ag
		JOIN accounts a ON a.id = ag.account_id
		WHERE ag.group_id = ANY($1)
			AND a.deleted_at IS NULL
			AND a.status = $2
			AND a.schedulable = TRUE
			AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= $3)
			AND (a.expires_at IS NULL OR a.expires_at > $3 OR a.auto_pause_on_expired = FALSE)
		ORDER BY ag.group_id ASC, ag.priority ASC, a.priority ASC, a.id ASC
	`, pq.Array(groupIDs), service.StatusActive, time.Now())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]service.GroupAccountCapacityRow, 0)
	for rows.Next() {
		var row service.GroupAccountCapacityRow
		var extraRaw string
		if err := rows.Scan(
			&row.GroupID,
			&row.AccountID,
			&row.Concurrency,
			&extraRaw,
			&row.SessionWindowStart,
			&row.SessionWindowEnd,
			&row.SessionWindowStatus,
			&row.RateLimitResetAt,
			&row.OverloadUntil,
			&row.WebImageRateLimitResetAt,
			&row.Platform,
			&row.Type,
		); err != nil {
			// Rolling migrate: web_image_rate_limit_reset_at may not exist yet — fail closed to empty.
			return nil, err
		}
		if extraRaw != "" && extraRaw != "null" {
			var extra map[string]any
			if err := json.Unmarshal([]byte(extraRaw), &extra); err != nil {
				return nil, err
			}
			row.Extra = extra
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SetWebImageRateLimited persists ChatGPT Web image cooldown on accounts (DB source of truth).
// Uses raw SQL so we do not need ent codegen for this fork-only field pair.
func (r *accountRepository) SetWebImageRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	if id <= 0 || resetAt.IsZero() {
		return nil
	}
	now := time.Now().UTC()
	resetAt = resetAt.UTC()
	// Only extend an existing window (never shorten) to avoid races between concurrent failures.
	_, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET web_image_rate_limited_at = COALESCE(web_image_rate_limited_at, $1),
			web_image_rate_limit_reset_at = CASE
				WHEN web_image_rate_limit_reset_at IS NULL OR web_image_rate_limit_reset_at < $2 THEN $2
				ELSE web_image_rate_limit_reset_at
			END,
			updated_at = NOW()
		WHERE id = $3
			AND deleted_at IS NULL
	`, now, resetAt, id)
	if err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue web image rate limit failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

// ClearWebImageRateLimit clears durable web-image cooldown columns.
func (r *accountRepository) ClearWebImageRateLimit(ctx context.Context, id int64) error {
	if id <= 0 {
		return nil
	}
	_, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET web_image_rate_limited_at = NULL,
			web_image_rate_limit_reset_at = NULL,
			updated_at = NOW()
		WHERE id = $1
			AND deleted_at IS NULL
	`, id)
	if err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue clear web image rate limit failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

// attachWebImageRateLimits loads durable web-image cooldown columns onto service accounts.
// Kept out of ent mapping to minimize upstream merge surface.
func (r *accountRepository) attachWebImageRateLimits(ctx context.Context, accounts []*service.Account) {
	if r == nil || r.sql == nil || len(accounts) == 0 {
		return
	}
	ids := make([]int64, 0, len(accounts))
	index := make(map[int64]*service.Account, len(accounts))
	for _, acc := range accounts {
		if acc == nil || acc.ID <= 0 {
			continue
		}
		if _, ok := index[acc.ID]; ok {
			continue
		}
		index[acc.ID] = acc
		ids = append(ids, acc.ID)
	}
	if len(ids) == 0 {
		return
	}
	// pgx/libpq friendly: ANY($1::bigint[])
	rows, err := r.sql.QueryContext(ctx, `
		SELECT id, web_image_rate_limited_at, web_image_rate_limit_reset_at
		FROM accounts
		WHERE deleted_at IS NULL
			AND id = ANY($1)
	`, pq.Array(ids))
	if err != nil {
		// Column may not exist yet during rolling migrate; fail open.
		logger.LegacyPrintf("repository.account", "attachWebImageRateLimits query failed: %v", err)
		return
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var limitedAt, resetAt *time.Time
		if err := rows.Scan(&id, &limitedAt, &resetAt); err != nil {
			continue
		}
		acc := index[id]
		if acc == nil {
			continue
		}
		acc.WebImageRateLimitedAt = limitedAt
		acc.WebImageRateLimitResetAt = resetAt
	}
}
