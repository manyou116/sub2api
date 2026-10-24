//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// testEntClient writes to the package's shared database without rollback. Track
// ownership before creating fixtures so cleanup also runs after a fatal assertion.
type capacityMonitorTestFixtures struct {
	groupIDs, userIDs, keyIDs, accountIDs, usageLogIDs []int64
	keyValues                                          []string
}

func trackCapacityMonitorTestFixtures(t *testing.T) *capacityMonitorTestFixtures {
	t.Helper()
	fixtures := &capacityMonitorTestFixtures{}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, cleanup := range []struct {
			query string
			ids   []int64
		}{
			{"DELETE FROM usage_logs WHERE id = ANY($1)", fixtures.usageLogIDs},
			{"DELETE FROM account_groups WHERE account_id = ANY($1)", fixtures.accountIDs},
			{"DELETE FROM api_keys WHERE id = ANY($1)", fixtures.keyIDs},
			{"DELETE FROM users WHERE id = ANY($1)", fixtures.userIDs},
			{"DELETE FROM accounts WHERE id = ANY($1)", fixtures.accountIDs},
			{"DELETE FROM groups WHERE id = ANY($1)", fixtures.groupIDs},
		} {
			if len(cleanup.ids) == 0 {
				continue
			}
			if _, err := integrationDB.ExecContext(ctx, cleanup.query, pq.Array(cleanup.ids)); err != nil {
				t.Errorf("clean capacity monitor fixtures (%s): %v", cleanup.query, err)
			}
		}
		// Deleting our API keys emits invalidations; remove only their exact hashes.
		if len(fixtures.keyValues) > 0 {
			_, err := integrationDB.ExecContext(ctx, `DELETE FROM auth_cache_invalidation_outbox
				WHERE cache_key IN (SELECT encode(sha256(convert_to(owned_key.value, 'UTF8')), 'hex') FROM unnest($1::text[]) AS owned_key(value))`, pq.Array(fixtures.keyValues))
			if err != nil {
				t.Errorf("clean capacity monitor key invalidations: %v", err)
			}
		}
	})
	return fixtures
}

func TestGroupCapacityMonitorReadSnapshot_RealSQLScopesAndReadOnly(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	fixtures := trackCapacityMonitorTestFixtures(t)
	group := mustCreateGroup(t, client, &service.Group{Name: "capacity-monitor-target", Platform: service.PlatformOpenAI})
	fixtures.groupIDs = append(fixtures.groupIDs, group.ID)
	other := mustCreateGroup(t, client, &service.Group{Name: "capacity-monitor-shared", Platform: service.PlatformOpenAI})
	fixtures.groupIDs = append(fixtures.groupIDs, other.ID)
	user := mustCreateUser(t, client, &service.User{Email: uuid.NewString() + "@monitor.test"})
	fixtures.userIDs = append(fixtures.userIDs, user.ID)
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "monitor-" + uuid.NewString(), Name: "monitor"})
	fixtures.keyIDs = append(fixtures.keyIDs, key.ID)
	fixtures.keyValues = append(fixtures.keyValues, key.Key)
	now := time.Now().UTC()
	observed := now.Add(-30 * time.Minute)
	extra := map[string]any{
		"codex_usage_updated_at":  observed.Format(time.RFC3339Nano),
		"codex_5h_reset_at":       observed.Add(2 * time.Hour).Format(time.RFC3339Nano),
		"codex_7d_reset_at":       observed.Add(3 * 24 * time.Hour).Format(time.RFC3339Nano),
		"codex_5h_window_minutes": 300, "codex_7d_window_minutes": 10080,
		"codex_5h_used_percent": 50, "codex_7d_used_percent": 20,
		"plan_type": "plus",
	}
	current := mustCreateAccount(t, client, &service.Account{Name: "monitor-current", Platform: service.PlatformOpenAI, Extra: extra,
		Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "member-current", "access_token": "must-not-be-projected"}})
	fixtures.accountIDs = append(fixtures.accountIDs, current.ID)
	removed := mustCreateAccount(t, client, &service.Account{Name: "monitor-removed", Platform: service.PlatformOpenAI})
	fixtures.accountIDs = append(fixtures.accountIDs, removed.ID)
	foreign := mustCreateAccount(t, client, &service.Account{Name: "monitor-other-platform", Platform: service.PlatformAnthropic})
	fixtures.accountIDs = append(fixtures.accountIDs, foreign.ID)
	for _, binding := range [][2]int64{{current.ID, group.ID}, {current.ID, other.ID}, {foreign.ID, group.ID}} {
		_, err := integrationDB.ExecContext(ctx, "INSERT INTO account_groups(account_id,group_id,priority) VALUES($1,$2,1)", binding[0], binding[1])
		require.NoError(t, err)
	}

	// Direct SQL makes the fee snapshots explicit: account fees must differ
	// from both standard cost and user billing, with NULL fallback also covered.
	insert := func(accountID, groupID int64, at time.Time, model, mode string, standard, userCost float64, stats, multiplier any, images int) {
		t.Helper()
		var logID int64
		err := integrationDB.QueryRowContext(ctx, `INSERT INTO usage_logs
			(user_id,api_key_id,account_id,request_id,model,total_cost,actual_cost,account_stats_cost,account_rate_multiplier,group_id,billing_mode,inbound_endpoint,upstream_endpoint,image_count,image_size,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'/v1/responses','/v1/responses',$12,CASE WHEN $12 > 0 THEN '1K' ELSE NULL END,$13) RETURNING id`,
			user.ID, key.ID, accountID, uuid.NewString(), model, standard, userCost, stats, multiplier, groupID, mode, images, at).Scan(&logID)
		require.NoError(t, err)
		fixtures.usageLogIDs = append(fixtures.usageLogIDs, logID)
	}
	insert(current.ID, group.ID, observed.Add(-time.Hour), "gpt-5", "token", 10, 100, 2.0, 3.0, 0)     // account 6
	insert(current.ID, other.ID, observed.Add(-45*time.Minute), "gpt-5", "token", 4, 100, nil, nil, 0) // account 4
	insert(current.ID, other.ID, observed.Add(5*time.Minute), "gpt-5", "token", 9, 100, 1.0, 2.0, 0)   // post-sample 2
	insert(removed.ID, group.ID, observed.Add(-time.Hour), "gpt-5", "token", 3, 100, nil, nil, 0)      // route only 3
	insert(current.ID, group.ID, observed.Add(-time.Hour), "gpt-image-2", "image", 800, 800, nil, nil, 1)
	insert(foreign.ID, group.ID, observed.Add(-time.Hour), "claude-sonnet", "token", 900, 900, nil, nil, 0)
	insert(current.ID, group.ID, observed.Add(-time.Hour), "monitor-hidden-spark", "token", 500, 500, nil, nil, 0)
	_, err := integrationDB.ExecContext(ctx, "UPDATE usage_logs SET requested_model='gpt-5', upstream_model='gpt-5-codex-spark' WHERE account_id=$1 AND model='monitor-hidden-spark'", current.ID)
	require.NoError(t, err)
	insert(current.ID, group.ID, now.Add(-16*24*time.Hour), "gpt-5", "token", 700, 700, nil, nil, 0)
	insert(current.ID, group.ID, now.Add(time.Hour), "gpt-5", "token", 600, 600, nil, nil, 0)

	var beforeExtra string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT extra::text FROM accounts WHERE id=$1", current.ID).Scan(&beforeExtra))
	snapshot, err := NewGroupCapacityMonitorRepository(integrationDB).ReadSnapshot(ctx, group.ID, 7*24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, group.Name, snapshot.GroupName)
	require.Len(t, snapshot.Accounts, 2)
	var found *service.CapacityMonitorAccount
	for i := range snapshot.Accounts {
		if snapshot.Accounts[i].ID == current.ID {
			found = &snapshot.Accounts[i]
		}
	}
	require.NotNil(t, found)
	require.True(t, found.Shared)
	require.NotEmpty(t, found.IdentityKey)
	require.NotContains(t, fmt.Sprint(found), "must-not-be-projected")
	require.InDelta(t, 10, found.Window5Cost, 1e-8)
	require.InDelta(t, 10, found.Window7Cost, 1e-8)
	require.InDelta(t, 2, found.Post5Cost, 1e-8)
	require.InDelta(t, 2, found.Post7Cost, 1e-8)
	var pool, route, via, shared float64
	for _, bin := range snapshot.History {
		pool += bin.PoolAccountCost
		route += bin.GroupRouteCost
		via += bin.PoolViaGroupCost
		shared += bin.PoolOtherCost
		require.False(t, bin.StartAt.Before(snapshot.AsOf.Truncate(time.Hour).Add(-7*24*time.Hour)))
		require.False(t, bin.EndAt.After(snapshot.AsOf))
	}
	require.InDelta(t, 12, pool, 1e-8)
	require.InDelta(t, 9, route, 1e-8)
	require.InDelta(t, 6, via, 1e-8)
	require.InDelta(t, 6, shared, 1e-8)
	require.InDelta(t, pool, via+shared, 1e-8)
	var afterExtra string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT extra::text FROM accounts WHERE id=$1", current.ID).Scan(&afterExtra))
	var before, after any
	require.NoError(t, json.Unmarshal([]byte(beforeExtra), &before))
	require.NoError(t, json.Unmarshal([]byte(afterExtra), &after))
	require.Equal(t, before, after, "monitor GET must not update account snapshots")
	_, err = NewGroupCapacityMonitorRepository(integrationDB).ReadSnapshot(ctx, -999, 24*time.Hour)
	require.ErrorIs(t, err, service.ErrCapacityMonitorNotFound)
}

func TestGroupCapacityMonitorReadSnapshot_IdentitySeparatesWorkspaceMembers(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	fixtures := trackCapacityMonitorTestFixtures(t)
	group := mustCreateGroup(t, client, &service.Group{Name: "monitor-identity-" + uuid.NewString(), Platform: service.PlatformOpenAI})
	fixtures.groupIDs = append(fixtures.groupIDs, group.ID)
	ids := []int64{}
	for _, userID := range []string{"member-a", "member-b", "member-a"} {
		a := mustCreateAccount(t, client, &service.Account{Name: "identity-" + uuid.NewString(), Platform: service.PlatformOpenAI,
			Credentials: map[string]any{"chatgpt_account_id": "shared-workspace", "chatgpt_user_id": userID, "refresh_token": "not-projected"}})
		fixtures.accountIDs = append(fixtures.accountIDs, a.ID)
		ids = append(ids, a.ID)
		_, err := integrationDB.ExecContext(ctx, "INSERT INTO account_groups(account_id,group_id,priority) VALUES($1,$2,1)", a.ID, group.ID)
		require.NoError(t, err)
	}
	// A duplicate import outside this group can still consume the same quota.
	// It must be recognized even though it is absent from the member list.
	outside := mustCreateAccount(t, client, &service.Account{Name: "outside-identity-" + uuid.NewString(), Platform: service.PlatformOpenAI,
		Credentials: map[string]any{"chatgpt_account_id": "shared-workspace", "chatgpt_user_id": "member-b"}})
	fixtures.accountIDs = append(fixtures.accountIDs, outside.ID)
	snapshot, err := NewGroupCapacityMonitorRepository(integrationDB).ReadSnapshot(ctx, group.ID, 24*time.Hour)
	require.NoError(t, err)
	require.Len(t, snapshot.Accounts, 3)
	keys := map[int64]string{}
	for _, a := range snapshot.Accounts {
		keys[a.ID] = a.IdentityKey
		require.True(t, a.DuplicateIdentity, "duplicate identities inside or outside the group must be excluded from modeled supply")
		require.NotContains(t, fmt.Sprint(a), "not-projected")
	}
	require.NotEmpty(t, keys[ids[0]])
	require.NotEqual(t, keys[ids[0]], keys[ids[1]], "different team members do not share the same quota identity just because the workspace matches")
	require.Equal(t, keys[ids[0]], keys[ids[2]], "duplicate imports can be recognized and excluded from supply")
}
