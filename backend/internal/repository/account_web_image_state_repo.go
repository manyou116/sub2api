package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

var _ service.WebImageStateRepository = (*accountRepository)(nil)

func (r *accountRepository) WebImageCooldown(ctx context.Context, id int64) (*time.Time, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT web_image_rate_limit_reset_at FROM accounts WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	var until sql.NullTime
	if err := rows.Scan(&until); err != nil {
		return nil, err
	}
	if until.Valid {
		return &until.Time, nil
	}
	return nil, nil
}
func (r *accountRepository) SetWebImageCooldown(ctx context.Context, id int64, until time.Time) error {
	_, err := r.sql.ExecContext(ctx, `UPDATE accounts SET web_image_rate_limited_at=NOW(),web_image_rate_limit_reset_at=GREATEST(web_image_rate_limit_reset_at,$2),extra=jsonb_set(COALESCE(extra,'{}'::jsonb),'{web_image_cooldown_reason}','"quota_exhausted"'::jsonb) WHERE id=$1 AND deleted_at IS NULL AND parent_account_id IS NULL`, id, until)
	return err
}

func (r *accountRepository) WebImageCooldowns(ctx context.Context, ids []int64) (map[int64]service.WebImageCooldown, error) {
	result := make(map[int64]service.WebImageCooldown, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := r.sql.QueryContext(ctx, `SELECT id,web_image_rate_limit_reset_at,web_image_rate_limited_at,COALESCE(extra->>'web_image_cooldown_reason','legacy_cooldown') FROM accounts WHERE id=ANY($1) AND deleted_at IS NULL`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var until, observed sql.NullTime
		var reason string
		if err := rows.Scan(&id, &until, &observed, &reason); err != nil {
			return nil, err
		}
		record := service.WebImageCooldown{Reason: reason}
		if until.Valid {
			record.Until = &until.Time
		}
		if observed.Valid {
			record.ObservedAt = &observed.Time
		}
		result[id] = record
	}
	return result, rows.Err()
}
func (r *accountRepository) ClearWebImageCooldownIfObserved(ctx context.Context, id int64, observed service.WebImageCooldown) error {
	if observed.Until == nil {
		return nil
	}
	_, err := r.sql.ExecContext(ctx, `UPDATE accounts SET web_image_rate_limited_at=NULL,web_image_rate_limit_reset_at=NULL,extra=extra-'web_image_cooldown_reason' WHERE id=$1 AND deleted_at IS NULL AND web_image_rate_limit_reset_at=$2 AND web_image_rate_limited_at IS NOT DISTINCT FROM $3`, id, observed.Until, observed.ObservedAt)
	return err
}
