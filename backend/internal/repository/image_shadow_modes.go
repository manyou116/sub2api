package repository

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ConfiguredImageShadowModes reads only the two possible resource labels. Keep
// paused, inactive and errored children: health is not a mode-selection policy.
func (r *accountRepository) ConfiguredImageShadowModes(ctx context.Context, groupID *int64, includeGrouped bool) ([]string, error) {
	if r.sql == nil {
		return nil, errors.New("image shadow routing SQL executor is unavailable")
	}
	query := `SELECT DISTINCT a.quota_dimension FROM accounts a
WHERE a.deleted_at IS NULL AND a.parent_account_id IS NOT NULL
AND a.platform='openai' AND a.quota_dimension IN ('web_images','work_images')`
	var args []any
	if groupID != nil {
		query += ` AND EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id=a.id AND ag.group_id=$1)`
		args = append(args, *groupID)
	} else if !includeGrouped {
		query += ` AND NOT EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id=a.id)`
	}
	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	modes := make([]string, 0, 2)
	for rows.Next() {
		var dimension string
		if err := rows.Scan(&dimension); err != nil {
			return nil, err
		}
		mode := service.ImageShadowModeWeb
		if dimension == service.QuotaDimensionWorkImages {
			mode = service.ImageShadowModeWork
		}
		modes = append(modes, mode)
	}
	return modes, rows.Err()
}
