package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type groupCapacityMonitorRepository struct{ db *sql.DB }

func NewGroupCapacityMonitorRepository(db *sql.DB) service.GroupCapacityMonitorRepository {
	return &groupCapacityMonitorRepository{db: db}
}

// Restrict the model to explicitly identifiable text usage. Legacy unclassified
// records are reported separately rather than silently treated as text/zero.
const capacityMonitorTextUsage = `ul.billing_mode = 'token'
 AND COALESCE(ul.image_count,0) = 0 AND COALESCE(ul.image_output_tokens,0) = 0
 AND COALESCE(ul.video_count,0) = 0
 AND lower(concat_ws(' ',ul.requested_model,ul.model,ul.upstream_model)) NOT LIKE '%spark%'
 AND lower(concat_ws(' ',ul.requested_model,ul.model,ul.upstream_model)) NOT LIKE '%image%'
 AND lower(concat_ws(' ',ul.requested_model,ul.model,ul.upstream_model)) NOT LIKE '%sora%'`

const capacityMonitorAccountCost = `COALESCE(ul.account_stats_cost,ul.total_cost) * COALESCE(ul.account_rate_multiplier,1)`

func (r *groupCapacityMonitorRepository) ReadSnapshot(ctx context.Context, groupID int64, history time.Duration) (_ *service.GroupCapacityMonitorSnapshot, err error) {
	// Three bounded queries run sequentially in the same consistent snapshot.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stage := "begin snapshot"
	defer func() { err = capacityMonitorSnapshotError(ctx, stage, err) }()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout = '8s'`); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout = '1s'`); err != nil {
		return nil, err
	}
	snapshot := &service.GroupCapacityMonitorSnapshot{Accounts: []service.CapacityMonitorAccount{}, History: []service.CapacityMonitorHistory{}, SharedUsage: []service.CapacityMonitorSharedUsage{}}
	stage = "group"
	err = tx.QueryRowContext(ctx, `SELECT transaction_timestamp(),name FROM groups WHERE id=$1 AND deleted_at IS NULL`, groupID).Scan(&snapshot.AsOf, &snapshot.GroupName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrCapacityMonitorNotFound
	}
	if err != nil {
		return nil, err
	}
	stage = "member identities"
	// Extract JSON identity fields once per account and aggregate only identities
	// needed by this group, while retaining duplicates outside the current pool.
	rows, err := tx.QueryContext(ctx, `
 WITH members AS MATERIALIZED (
 SELECT a.id,a.platform,a.type,COALESCE(a.quota_dimension,'global') AS quota_dimension,a.parent_account_id,a.status,a.schedulable,
 a.expires_at,a.auto_pause_on_expired,a.rate_limit_reset_at,a.overload_until,a.temp_unschedulable_until,COALESCE(a.temp_unschedulable_reason,'') AS temp_reason,
 CASE WHEN COALESCE(f.chatgpt_account_id,'')<>'' AND COALESCE(f.chatgpt_user_id,'')<>''
 THEN md5(f.chatgpt_account_id||chr(31)||f.chatgpt_user_id||chr(31)||COALESCE(a.quota_dimension,'global')) ELSE '' END AS identity_key,
 CASE WHEN COALESCE(f.plan_type,'')<>'' THEN f.plan_type||'|'||a.rate_multiplier::text ELSE '' END AS plan,
 jsonb_build_object('codex_usage_updated_at',a.extra->'codex_usage_updated_at',
 'codex_5h_used_percent',a.extra->'codex_5h_used_percent','codex_7d_used_percent',a.extra->'codex_7d_used_percent',
 'codex_5h_reset_at',a.extra->'codex_5h_reset_at','codex_7d_reset_at',a.extra->'codex_7d_reset_at',
 'codex_5h_window_minutes',a.extra->'codex_5h_window_minutes','codex_7d_window_minutes',a.extra->'codex_7d_window_minutes') AS extra,
 EXISTS(SELECT 1 FROM account_groups shared WHERE shared.account_id=a.id AND shared.group_id<>$1) AS shared
 FROM accounts a JOIN account_groups ag ON ag.account_id=a.id
 CROSS JOIN LATERAL jsonb_to_record(CASE WHEN jsonb_typeof(a.credentials)='object' THEN a.credentials ELSE '{}'::jsonb END)
 AS f(chatgpt_account_id text,chatgpt_user_id text,plan_type text)
 WHERE ag.group_id=$1 AND a.deleted_at IS NULL ORDER BY a.id LIMIT 1001
 ), needed AS MATERIALIZED (
 SELECT DISTINCT identity_key FROM members WHERE platform='openai' AND type='oauth' AND parent_account_id IS NULL AND identity_key<>''
 ), provider_keys AS MATERIALIZED (
 SELECT md5(f.chatgpt_account_id||chr(31)||f.chatgpt_user_id||chr(31)||COALESCE(a.quota_dimension,'global')) AS identity_key
 FROM accounts a CROSS JOIN LATERAL jsonb_to_record(CASE WHEN jsonb_typeof(a.credentials)='object' THEN a.credentials ELSE '{}'::jsonb END)
 AS f(chatgpt_account_id text,chatgpt_user_id text)
 WHERE a.deleted_at IS NULL AND a.platform='openai' AND a.type='oauth' AND a.parent_account_id IS NULL
 AND COALESCE(f.chatgpt_account_id,'')<>'' AND COALESCE(f.chatgpt_user_id,'')<>'' AND EXISTS(SELECT 1 FROM needed)
 ), duplicates AS MATERIALIZED (
 SELECT p.identity_key,COUNT(*) AS copies FROM provider_keys p JOIN needed n ON n.identity_key=p.identity_key GROUP BY p.identity_key
 )
 SELECT m.*,COALESCE(d.copies,0)>1 FROM members m LEFT JOIN duplicates d ON d.identity_key=m.identity_key ORDER BY m.id`, groupID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a service.CapacityMonitorAccount
		var extra []byte
		if err = rows.Scan(&a.ID, &a.Platform, &a.Type, &a.QuotaDimension, &a.ParentID, &a.Status, &a.Schedulable, &a.ExpiresAt, &a.AutoPauseOnExpired, &a.RateResetAt, &a.OverloadUntil, &a.TempUntil, &a.TempReason, &a.IdentityKey, &a.Plan, &extra, &a.Shared, &a.DuplicateIdentity); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err = json.Unmarshal(extra, &a.Extra); err != nil {
			_ = rows.Close()
			return nil, err
		}
		snapshot.Accounts = append(snapshot.Accounts, a)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if len(snapshot.Accounts) > 1000 {
		return nil, service.ErrCapacityMonitorBudget
	}
	stage = "window costs"
	if err = r.readMonitorWindowCosts(ctx, tx, snapshot); err != nil {
		return nil, err
	}
	end := snapshot.AsOf.UTC().Truncate(time.Hour)
	start := end.Add(-history)
	ids := []int64{}
	for _, a := range snapshot.Accounts {
		if a.Platform == service.PlatformOpenAI && a.Type == service.AccountTypeOAuth && a.ParentID == nil && (a.QuotaDimension == "global" || a.QuotaDimension == "") {
			ids = append(ids, a.ID)
		}
	}
	stage = "history"
	if err = r.readMonitorHistory(ctx, tx, snapshot, groupID, ids, start); err != nil {
		return nil, err
	}
	stage = "commit"
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func capacityMonitorSnapshotError(ctx context.Context, stage string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pq.Error
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	} else if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		err = errors.Join(err, context.DeadlineExceeded)
	}
	return fmt.Errorf("capacity monitor %s: %w", stage, err)
}

func (r *groupCapacityMonitorRepository) readMonitorWindowCosts(ctx context.Context, tx *sql.Tx, snapshot *service.GroupCapacityMonitorSnapshot) error {
	type bounds struct {
		ID       int64     `json:"id"`
		Observed time.Time `json:"observed"`
		Start5   time.Time `json:"start5"`
		Start7   time.Time `json:"start7"`
		Post5    time.Time `json:"post5"`
		Post7    time.Time `json:"post7"`
	}
	windows := []bounds{}
	indexes := map[int64]int{}
	for i, a := range snapshot.Accounts {
		observed, start5, start7, ok := service.CapacityMonitorSampleBounds(a)
		if !ok || observed.After(snapshot.AsOf) || snapshot.AsOf.Sub(observed) > 2*time.Hour {
			continue
		}
		post5, post7 := observed, observed
		if reset := start5.Add(5 * time.Hour); !reset.After(snapshot.AsOf) {
			post5 = reset
		}
		if reset := start7.Add(7 * 24 * time.Hour); !reset.After(snapshot.AsOf) {
			post7 = reset
		}
		windows = append(windows, bounds{a.ID, observed, start5, start7, post5, post7})
		indexes[a.ID] = i
	}
	if len(windows) == 0 {
		return nil
	}
	raw, err := json.Marshal(windows)
	if err != nil {
		return err
	}
	query := `WITH windows AS (SELECT * FROM jsonb_to_recordset($1::jsonb) AS w(id bigint,observed timestamptz,start5 timestamptz,start7 timestamptz,post5 timestamptz,post7 timestamptz))
 SELECT w.id,
 COALESCE(SUM(` + capacityMonitorAccountCost + `) FILTER (WHERE ul.created_at>=w.start5 AND ul.created_at<w.observed),0),
 COALESCE(SUM(` + capacityMonitorAccountCost + `) FILTER (WHERE ul.created_at>=w.start7 AND ul.created_at<w.observed),0),
 COALESCE(SUM(` + capacityMonitorAccountCost + `) FILTER (WHERE ul.created_at>=w.post5),0),
 COALESCE(SUM(` + capacityMonitorAccountCost + `) FILTER (WHERE ul.created_at>=w.post7),0)
 FROM windows w LEFT JOIN usage_logs ul ON ul.account_id=w.id
 AND ul.created_at>=LEAST(w.start5,w.start7) AND ul.created_at<$2 AND (` + capacityMonitorTextUsage + `)
 GROUP BY w.id`
	rows, err := tx.QueryContext(ctx, query, string(raw), snapshot.AsOf)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var c5, c7, post5, post7 float64
		if err = rows.Scan(&id, &c5, &c7, &post5, &post7); err != nil {
			return err
		}
		i := indexes[id]
		snapshot.Accounts[i].Window5Cost = c5
		snapshot.Accounts[i].Window7Cost = c7
		snapshot.Accounts[i].Post5Cost = post5
		snapshot.Accounts[i].Post7Cost = post7
	}
	return rows.Err()
}

func (r *groupCapacityMonitorRepository) readMonitorHistory(ctx context.Context, tx *sql.Tx, snapshot *service.GroupCapacityMonitorSnapshot, groupID int64, ids []int64, start time.Time) error {
	// Pool IDs are already eligible, so only route-only former members require
	// an account join. Excluding pool IDs keeps UNION ALL records disjoint.
	query := `WITH records AS MATERIALIZED (
 SELECT ul.created_at,ul.group_id,TRUE AS in_pool,(` + capacityMonitorTextUsage + `) AS is_text,
 (` + capacityMonitorAccountCost + `) AS account_cost
 FROM usage_logs ul
 WHERE ul.created_at>=$2 AND ul.created_at<$3
 AND ul.account_id=ANY($1::bigint[])
 UNION ALL
 SELECT ul.created_at,ul.group_id,FALSE AS in_pool,(` + capacityMonitorTextUsage + `) AS is_text,
 (` + capacityMonitorAccountCost + `) AS account_cost
 FROM usage_logs ul JOIN accounts a ON a.id=ul.account_id
 WHERE ul.created_at>=$2 AND ul.created_at<$3 AND ul.group_id=$4
 AND NOT(ul.account_id=ANY($1::bigint[]))
 AND a.platform='openai' AND a.type='oauth' AND a.parent_account_id IS NULL AND COALESCE(a.quota_dimension,'global')='global'
 )
 SELECT date_trunc('hour',records.created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS bucket,group_id,COALESCE(g.name,''),
 COALESCE(SUM(account_cost) FILTER(WHERE in_pool AND is_text),0),
 COALESCE(SUM(account_cost) FILTER(WHERE group_id=$4 AND is_text),0),
 COUNT(*) FILTER(WHERE in_pool AND is_text),COUNT(*) FILTER(WHERE group_id=$4 AND is_text),
 COALESCE(SUM(account_cost) FILTER(WHERE in_pool AND NOT COALESCE(is_text,false)),0)
 FROM records LEFT JOIN groups g ON g.id=records.group_id
 GROUP BY bucket,group_id,g.name ORDER BY bucket,group_id`
	rows, err := tx.QueryContext(ctx, query, pq.Array(ids), start, snapshot.AsOf, groupID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	buckets := map[int64]*service.CapacityMonitorHistory{}
	for at := start; at.Before(snapshot.AsOf); at = at.Add(time.Hour) {
		end := at.Add(time.Hour)
		complete := !end.After(snapshot.AsOf)
		if !complete {
			end = snapshot.AsOf
		}
		p := service.CapacityMonitorHistory{StartAt: at, EndAt: end, Complete: complete}
		snapshot.History = append(snapshot.History, p)
	}
	for i := range snapshot.History {
		p := &snapshot.History[i]
		buckets[p.StartAt.Unix()] = p
	}
	for rows.Next() {
		var at time.Time
		var gid sql.NullInt64
		var name string
		var pool, route, unknown float64
		var poolRequests, routeRequests int64
		if err = rows.Scan(&at, &gid, &name, &pool, &route, &poolRequests, &routeRequests, &unknown); err != nil {
			return err
		}
		p := buckets[at.Unix()]
		if p == nil {
			return fmt.Errorf("capacity monitor bucket outside range")
		}
		p.PoolAccountCost += pool
		p.GroupRouteCost += route
		p.PoolRequests += poolRequests
		p.GroupRouteRequests += routeRequests
		if gid.Valid && gid.Int64 == groupID {
			p.PoolViaGroupCost += pool
		} else {
			p.PoolOtherCost += pool
		}
		snapshot.UnclassifiedCost += unknown
		if pool != 0 {
			entry := service.CapacityMonitorSharedUsage{GroupName: name, Cost: pool, BucketStart: at}
			if gid.Valid {
				id := gid.Int64
				entry.GroupID = &id
			}
			snapshot.SharedUsage = append(snapshot.SharedUsage, entry)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return nil
}
