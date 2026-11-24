package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *accountRepository) RecentOpenAIQualitySamples(ctx context.Context, since time.Time) ([]service.OpenAIQualitySample, error) {
	rows, err := r.sql.QueryContext(ctx, `
		WITH plan_results AS (
			SELECT p.account_id, p.model_id, p.quality_priority,
				count(*) FILTER (WHERE v.quality_verdict = 'pass') AS passed,
				count(*) AS total,
				p.quality_isolation_failures > 0
					AND count(*) >= p.quality_isolation_failures
					AND NOT ('pass' = ANY((array_agg(v.quality_verdict ORDER BY v.finished_at DESC, v.id DESC))[1:p.quality_isolation_failures]))
					AS isolated
			FROM scheduled_test_plans p JOIN accounts a ON a.id = p.account_id
			CROSS JOIN LATERAL (
				SELECT id, finished_at, quality_verdict FROM scheduled_test_results
				WHERE plan_id = p.id AND quality_revision = p.quality_revision AND finished_at >= $1
				AND quality_verdict IN ('pass', 'fail')
				ORDER BY finished_at DESC, id DESC LIMIT 8
			) v
			WHERE p.enabled AND (p.quality_priority OR p.quality_isolation_failures > 0)
				AND p.quality_prompt <> '' AND a.platform = 'openai' AND a.deleted_at IS NULL
			GROUP BY p.id, p.account_id, p.model_id, p.quality_priority, p.quality_isolation_failures
		)
		SELECT account_id, model_id, coalesce(sum(passed) FILTER (WHERE quality_priority), 0),
			coalesce(sum(total) FILTER (WHERE quality_priority), 0), bool_or(isolated)
		FROM plan_results GROUP BY account_id, model_id ORDER BY account_id, model_id LIMIT 10000`, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var samples []service.OpenAIQualitySample
	for rows.Next() {
		var sample service.OpenAIQualitySample
		if err = rows.Scan(&sample.AccountID, &sample.Model, &sample.Passed, &sample.Total, &sample.Isolated); err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
	return samples, rows.Err()
}
