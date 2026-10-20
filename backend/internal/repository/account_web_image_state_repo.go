package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
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
	_, err := r.sql.ExecContext(ctx, `UPDATE accounts SET web_image_rate_limited_at=NOW(),web_image_rate_limit_reset_at=GREATEST(web_image_rate_limit_reset_at,$2) WHERE id=$1 AND deleted_at IS NULL AND parent_account_id IS NULL`, id, until)
	return err
}
