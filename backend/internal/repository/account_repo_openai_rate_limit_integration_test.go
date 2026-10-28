//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func createReconcileAccount(t *testing.T, client *dbent.Client, name string) *service.Account {
	t.Helper()
	limited, reset := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	return mustCreateAccount(t, client, &service.Account{
		Name: name, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true,
		Credentials:   map[string]any{"access_token": "test-only"},
		Extra:         map[string]any{service.OpenAIRateLimitReconcileEnabledExtraKey: true},
		RateLimitedAt: &limited, RateLimitResetAt: &reset,
	})
}

func TestOpenAIReconcileRepositoryBoundedClaimsAndFairness(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	for i := 0; i < 25; i++ {
		createReconcileAccount(t, tx.Client(), fmt.Sprintf("reconcile-fair-%d", i))
	}
	first, err := repo.ClaimOpenAIRateLimitReconcileAccounts(ctx, time.Now(), 100, "first")
	require.NoError(t, err)
	require.Len(t, first, 20, "repository enforces the cap independently of the caller")
	seen := map[int64]bool{}
	for _, a := range first {
		seen[a.ID] = true
		state := a.Extra[service.OpenAIRateLimitReconcileStateExtraKey].(map[string]any)
		require.Equal(t, "first", state["lease"])
		require.Greater(t, state["next_check_unix"].(float64), float64(time.Now().Add(9*time.Minute).Unix()))
	}
	second, err := repo.ClaimOpenAIRateLimitReconcileAccounts(ctx, time.Now(), 20, "second")
	require.NoError(t, err)
	require.Len(t, second, 5)
	for _, a := range second {
		require.False(t, seen[a.ID], "a second instance cannot re-claim a leased account")
	}
	third, err := repo.ClaimOpenAIRateLimitReconcileAccounts(ctx, time.Now(), 20, "third")
	require.NoError(t, err)
	require.Empty(t, third)
	later, err := repo.ClaimOpenAIRateLimitReconcileAccounts(ctx, time.Now().Add(11*time.Minute), 20, "later")
	require.NoError(t, err)
	require.Len(t, later, 20, "claims become eligible after a crashed worker's persisted delay")
}

func TestOpenAIReconcileRepositoryFiltersAndMalformedDueTime(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	for _, scenario := range []string{"off", "apikey", "shadow", "inactive", "unschedulable", "not-limited", "expired", "overloaded", "temporary", "not-due", "deleted"} {
		a := createReconcileAccount(t, tx.Client(), "reconcile-filter-"+scenario)
		b := tx.Client().Account.UpdateOneID(a.ID)
		switch scenario {
		case "off":
			b.SetExtra(map[string]any{})
		case "apikey":
			b.SetType(service.AccountTypeAPIKey)
		case "shadow":
			parent := mustCreateAccount(t, tx.Client(), &service.Account{Name: "parent", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth})
			b.SetParentAccountID(parent.ID).SetQuotaDimension(dbaccount.QuotaDimensionSpark)
		case "inactive":
			b.SetStatus(service.StatusError)
		case "unschedulable":
			b.SetSchedulable(false)
		case "not-limited":
			b.ClearRateLimitedAt().ClearRateLimitResetAt()
		case "expired":
			b.SetExpiresAt(time.Now().Add(-time.Hour)).SetAutoPauseOnExpired(true)
		case "overloaded":
			b.SetOverloadUntil(time.Now().Add(time.Hour))
		case "temporary":
			b.SetTempUnschedulableUntil(time.Now().Add(time.Hour))
		case "not-due":
			b.SetExtra(map[string]any{service.OpenAIRateLimitReconcileEnabledExtraKey: true, service.OpenAIRateLimitReconcileStateExtraKey: map[string]any{"next_check_unix": time.Now().Add(time.Hour).Unix()}})
		case "deleted":
			b.SetDeletedAt(time.Now())
		}
		_, err := b.Save(ctx)
		require.NoError(t, err)
	}
	ids := map[int64]bool{}
	for i, v := range []any{"invalid", "999999999999999999999999999999", nil, -1} {
		a := createReconcileAccount(t, tx.Client(), fmt.Sprintf("malformed-due-%d", i))
		require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.OpenAIRateLimitReconcileStateExtraKey: map[string]any{"next_check_unix": v}}))
		ids[a.ID] = true
	}
	claimed, err := repo.ClaimOpenAIRateLimitReconcileAccounts(ctx, time.Now(), 20, "filters")
	require.NoError(t, err)
	require.Len(t, claimed, 4)
	for _, a := range claimed {
		require.True(t, ids[a.ID])
	}
}

func TestOpenAIReconcileRepositoryCompletionCAS(t *testing.T) {
	for _, scenario := range []string{"recover", "still-limited", "new429", "disabled", "disable-then-enable", "credentials", "proxy", "manual-stop", "overload", "temporary", "reclaimed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			tx := testEntTx(t)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			a := createReconcileAccount(t, tx.Client(), "reconcile-cas-"+scenario)
			expired := time.Now().Add(-time.Hour)
			_, err := tx.Client().Account.UpdateOneID(a.ID).SetOverloadUntil(expired).SetTempUnschedulableUntil(expired).Save(ctx)
			require.NoError(t, err)
			require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{"model_rate_limits": map[string]any{"model": "keep"}, "openai_web_images": map[string]any{"remaining": 0}}))
			claimed, err := repo.ClaimOpenAIRateLimitReconcileAccounts(ctx, time.Now(), 1, "generation")
			require.NoError(t, err)
			require.Len(t, claimed, 1)
			observed := claimed[0]
			b := tx.Client().Account.UpdateOneID(a.ID)
			switch scenario {
			case "new429":
				b.SetRateLimitedAt(time.Now()).SetRateLimitResetAt(time.Now().Add(2 * time.Hour))
			case "disabled":
				require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.OpenAIRateLimitReconcileEnabledExtraKey: false}))
				b = nil
			case "disable-then-enable":
				require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.OpenAIRateLimitReconcileEnabledExtraKey: false, service.OpenAIRateLimitReconcileStateExtraKey: nil}))
				require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.OpenAIRateLimitReconcileEnabledExtraKey: true, service.OpenAIRateLimitReconcileStateExtraKey: nil}))
				b = nil
			case "credentials":
				b.SetCredentials(map[string]any{"access_token": "changed"})
			case "proxy":
				proxy := mustCreateProxy(t, tx.Client(), &service.Proxy{Name: "reconcile-proxy"})
				b.SetProxyID(proxy.ID)
			case "manual-stop":
				b.SetSchedulable(false)
			case "overload":
				b.SetOverloadUntil(time.Now().Add(time.Hour))
			case "temporary":
				b.SetTempUnschedulableUntil(time.Now().Add(time.Hour))
			case "reclaimed":
				require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.OpenAIRateLimitReconcileStateExtraKey: map[string]any{"lease": "other"}}))
				b = nil
			default:
				b = nil
			}
			if b != nil {
				_, err = b.Save(ctx)
				require.NoError(t, err)
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM scheduler_outbox WHERE account_id = $1`, a.ID)
			require.NoError(t, err)
			updates := map[string]any{"codex_5h_used_percent": 0, service.OpenAIRateLimitReconcileStateExtraKey: map[string]any{"status": "recovered", "next_check_unix": time.Now().Add(10 * time.Minute).Unix()}}
			applied, err := repo.CompleteOpenAIRateLimitReconcile(ctx, observed, updates, scenario != "still-limited")
			require.NoError(t, err)
			want := scenario == "recover" || scenario == "still-limited"
			require.Equal(t, want, applied)
			after, err := repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			if scenario == "recover" {
				require.Nil(t, after.RateLimitedAt)
				require.Nil(t, after.RateLimitResetAt)
			} else {
				require.NotNil(t, after.RateLimitResetAt)
			}
			_, hasUsage := after.Extra["codex_5h_used_percent"]
			require.Equal(t, want, hasUsage, "stale probes must not overwrite official usage snapshots")
			require.Equal(t, map[string]any{"model": "keep"}, after.Extra["model_rate_limits"])
			require.Equal(t, map[string]any{"remaining": float64(0)}, after.Extra["openai_web_images"])
			if scenario == "recover" {
				require.Equal(t, observed.OverloadUntil, after.OverloadUntil)
				require.Equal(t, observed.TempUnschedulableUntil, after.TempUnschedulableUntil)
				rows, err := tx.QueryContext(ctx, `SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1`, a.ID)
				require.NoError(t, err)
				require.True(t, rows.Next())
				var count int
				require.NoError(t, rows.Scan(&count))
				_ = rows.Close()
				require.Equal(t, 1, count)
			}
		})
	}
}

func TestOpenAIReconcileRepositoryOrdinaryEditPreservesLatestState(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	a := createReconcileAccount(t, tx.Client(), "reconcile-edit")
	stale, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	claimed, err := repo.ClaimOpenAIRateLimitReconcileAccounts(ctx, time.Now(), 1, "latest")
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	stale.Extra = map[string]any{"custom": "edit"}
	require.NoError(t, repo.Update(ctx, stale))
	after, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, true, after.Extra[service.OpenAIRateLimitReconcileEnabledExtraKey])
	require.Equal(t, claimed[0].Extra[service.OpenAIRateLimitReconcileStateExtraKey], after.Extra[service.OpenAIRateLimitReconcileStateExtraKey])
	require.Equal(t, "edit", after.Extra["custom"])
	stale = after
	claimed, err = repo.ClaimOpenAIRateLimitReconcileAccounts(ctx, time.Now().Add(11*time.Minute), 1, "recovered")
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	applied, err := repo.CompleteOpenAIRateLimitReconcile(ctx, claimed[0], map[string]any{"codex_5h_used_percent": 0}, true)
	require.NoError(t, err)
	require.True(t, applied)
	stale.Extra["codex_5h_used_percent"] = 100
	require.NoError(t, repo.Update(ctx, stale))
	after, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Nil(t, after.RateLimitedAt, "ordinary edits must not resurrect a recovered cooldown")
	require.Nil(t, after.RateLimitResetAt)
	require.Equal(t, float64(0), after.Extra["codex_5h_used_percent"])
	stale = after
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.OpenAIRateLimitReconcileEnabledExtraKey: false, service.OpenAIRateLimitReconcileStateExtraKey: nil}))
	require.NoError(t, repo.Update(ctx, stale))
	after, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, false, after.Extra[service.OpenAIRateLimitReconcileEnabledExtraKey], "a stale edit cannot re-enable background checks")
}

func TestOpenAIReconcileRepositoryConcurrentClaimsSkipLocked(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	a := createReconcileAccount(t, client, "reconcile-concurrent")
	t.Cleanup(func() { _ = client.Account.DeleteOneID(a.ID).Exec(ctx) })
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	firstRepo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	secondRepo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	first, err := firstRepo.ClaimOpenAIRateLimitReconcileAccounts(ctx, time.Now(), 1, "holding-row-lock")
	require.NoError(t, err)
	require.Len(t, first, 1)
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	second, err := secondRepo.ClaimOpenAIRateLimitReconcileAccounts(bounded, time.Now(), 1, "peer")
	require.NoError(t, err)
	require.Empty(t, second, "a peer must skip the uncommitted claim without waiting on its row lock")
	require.NoError(t, tx.Rollback())
	second, err = secondRepo.ClaimOpenAIRateLimitReconcileAccounts(ctx, time.Now(), 1, "peer-after-rollback")
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, a.ID, second[0].ID)
	third, err := secondRepo.ClaimOpenAIRateLimitReconcileAccounts(ctx, time.Now(), 1, "third")
	require.NoError(t, err)
	require.Empty(t, third, "committed claims exclude peers until their persisted next-check time")
}
