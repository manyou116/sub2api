package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Scheduling needs the current privacy policy, but not the group's account
// counts. Keep the expensive repository method unusable in these regressions.
type schedulerPrivacyLiteGroupRepo struct {
	GroupRepository
	group     *Group
	err       error
	liteCalls int
}

func (*schedulerPrivacyLiteGroupRepo) GetByID(context.Context, int64) (*Group, error) {
	panic("scheduler privacy must not aggregate group account counts")
}

func (r *schedulerPrivacyLiteGroupRepo) GetByIDLite(context.Context, int64) (*Group, error) {
	r.liteCalls++
	if r.err != nil {
		return nil, r.err
	}
	return r.group, nil
}

func TestOpenAIGatewayService_SchedulerPrivacyUsesLiteGroupAcrossModes(t *testing.T) {
	const groupID int64 = 103001
	for _, mode := range []struct {
		name      string
		advanced  string
		loadBatch bool
	}{
		{name: "legacy", advanced: "false"},
		{name: "legacy load batch", advanced: "false", loadBatch: true},
		{name: "advanced", advanced: "true", loadBatch: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, policy := range []struct {
				name     string
				required bool
				err      error
				wantID   int64
			}{
				{name: "required", required: true, wantID: 39102},
				{name: "optional", wantID: 39101},
				{name: "lookup error fails closed", err: errors.New("group unavailable"), wantID: 39102},
			} {
				t.Run(policy.name, func(t *testing.T) {
					resetOpenAIAdvancedSchedulerSettingCacheForTest()
					t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
					accounts := []Account{
						{ID: 39101, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}, Credentials: map[string]any{"plan_type": "team"}},
						{ID: 39102, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}, Credentials: map[string]any{"plan_type": "team"}, Extra: map[string]any{"privacy_mode": PrivacyModeTrainingOff}},
					}
					accountRepo := &guardianAffinityAccountRepo{schedulerGroupAwareOpenAIAccountRepo: schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}}}
					groupRepo := &schedulerPrivacyLiteGroupRepo{
						group: &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, RequirePrivacySet: policy.required},
						err:   policy.err,
					}
					cfg := &config.Config{}
					cfg.Gateway.Scheduling.LoadBatchEnabled = mode.loadBatch
					cfg.Gateway.OpenAIWS.LBTopK = 1
					cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
					svc := &OpenAIGatewayService{
						accountRepo:        accountRepo,
						cache:              &schedulerTestGatewayCache{},
						cfg:                cfg,
						rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(mode.advanced),
						concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
						schedulerSnapshot:  &SchedulerSnapshotService{accountRepo: accountRepo, groupRepo: groupRepo},
					}
					selectAccount := func(wantID int64) {
						t.Helper()
						id := groupID
						selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &id, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
						require.NoError(t, err)
						require.NotNil(t, selection)
						require.Equal(t, wantID, selection.Account.ID)
						if selection.ReleaseFunc != nil {
							selection.ReleaseFunc()
						}
					}
					selectAccount(policy.wantID)
					require.Positive(t, groupRepo.liteCalls)
					require.Zero(t, accountRepo.setErrorCalls, "group privacy must not disable a shared account")
					require.True(t, accounts[0].Schedulable)
					require.Equal(t, StatusActive, accounts[0].Status)

					// A new request must see policy changes and database recovery;
					// switching to the lighter query must not introduce stale policy.
					groupRepo.err = nil
					groupRepo.group.RequirePrivacySet = policy.wantID == 39101
					callsBefore := groupRepo.liteCalls
					wantAfterChange := int64(39101)
					if groupRepo.group.RequirePrivacySet {
						wantAfterChange = 39102
					}
					selectAccount(wantAfterChange)
					require.Greater(t, groupRepo.liteCalls, callsBefore)
				})
			}
		})
	}
}

func TestOpenAISchedulerLoadBalanceAvoidsOnlyRedundantPrivacyLookup(t *testing.T) {
	for _, tc := range []struct {
		name            string
		requestRequired bool
		groupRequired   bool
		lookupErr       error
		wantID          int64
		wantCalls       int
		wantCandidates  int
	}{
		{name: "required request and required group", requestRequired: true, groupRequired: true, wantID: 39202, wantCandidates: 1},
		{name: "required request and optional group", requestRequired: true, wantID: 39202, wantCandidates: 1},
		{name: "required request and failing lookup", requestRequired: true, lookupErr: errors.New("group unavailable"), wantID: 39202, wantCandidates: 1},
		{name: "optional request sees newly required group", groupRequired: true, wantID: 39202, wantCalls: 1, wantCandidates: 1},
		{name: "optional request and optional group", wantID: 39201, wantCalls: 1, wantCandidates: 2},
		{name: "optional request preserves failed second lookup", lookupErr: errors.New("group unavailable"), wantID: 39201, wantCalls: 1, wantCandidates: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(103002)
			accounts := []Account{
				{ID: 39201, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}, Credentials: map[string]any{"plan_type": "team"}},
				{ID: 39202, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}, Credentials: map[string]any{"plan_type": "team"}, Extra: map[string]any{"privacy_mode": PrivacyModeTrainingOff}},
			}
			groupRepo := &schedulerPrivacyLiteGroupRepo{
				group: &Group{ID: groupID, RequirePrivacySet: tc.groupRequired},
				err:   tc.lookupErr,
			}
			accountRepo := schedulerTestOpenAIAccountRepo{accounts: accounts}
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.LBTopK = 1
			cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
			svc := &OpenAIGatewayService{
				accountRepo: accountRepo,
				cfg:         cfg,
				schedulerSnapshot: &SchedulerSnapshotService{
					accountRepo: accountRepo, groupRepo: groupRepo,
					cache: &openAISnapshotCacheStub{
						snapshotAccounts: []*Account{&accounts[0], &accounts[1]},
						accountsByID:     map[int64]*Account{39201: &accounts[0], 39202: &accounts[1]},
					},
				},
			}
			// Simulate the policy captured at request entry. A later lookup error
			// must retain the existing initial-filter behavior; errors on the first
			// lookup still fail closed in the across-modes regression above.
			ctx := context.WithValue(context.Background(), openAIGroupPrivacyRequirementContextKey{}, openAIGroupPrivacyRequirement{
				groupID: groupID, required: tc.requestRequired,
			})
			scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: newOpenAIAccountRuntimeStats()}
			selection, candidates, _, _, err := scheduler.selectByLoadBalance(ctx, OpenAIAccountScheduleRequest{
				GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "gpt-5.1", RequirePrivacySet: tc.requestRequired,
			})
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, tc.wantID, selection.Account.ID)
			require.Equal(t, tc.wantCandidates, candidates)
			require.Equal(t, tc.wantCalls, groupRepo.liteCalls)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}
