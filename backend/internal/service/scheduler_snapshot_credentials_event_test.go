//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type credentialsEventAccountRepo struct {
	*batchAccountQueryRepo
	account *Account
	getErr  error
	reads   int
}

func (r *credentialsEventAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	r.reads++
	return r.account, r.getErr
}

type credentialsEventSnapshotCache struct {
	*bulkEventSnapshotCache
	setErr  error
	account *Account
}

func (c *credentialsEventSnapshotCache) SetAccount(ctx context.Context, account *Account) error {
	if c.setErr != nil {
		return c.setErr
	}
	c.account = account
	return c.bulkEventSnapshotCache.SetAccount(ctx, account)
}

func newCredentialsEventService() (*SchedulerSnapshotService, *credentialsEventSnapshotCache, *credentialsEventAccountRepo) {
	cache := &credentialsEventSnapshotCache{bulkEventSnapshotCache: newBulkEventSnapshotCache()}
	repo := &credentialsEventAccountRepo{
		batchAccountQueryRepo: newBatchAccountQueryRepo(),
		account: &Account{
			ID: 71, Platform: PlatformOpenAI, GroupIDs: []int64{15},
			Credentials: map[string]any{"access_token": "refreshed", "model_mapping": map[string]any{"requested": "upstream"}},
		},
	}
	return newBulkEventTestService(cache, repo), cache, repo
}

func TestSchedulerCredentialEventRefreshesPayloadWithoutGroupRebuild(t *testing.T) {
	svc, cache, repo := newCredentialsEventService()
	seen := make(map[batchSeenKey]struct{})
	err := svc.handleOutboxEvent(context.Background(), SchedulerOutboxEvent{
		EventType: SchedulerOutboxEventAccountChanged, AccountID: &repo.account.ID,
		Payload: map[string]any{"cache_only": true},
	}, seen)

	require.NoError(t, err)
	require.Equal(t, 1, repo.reads)
	require.Equal(t, repo.account, cache.account)
	require.Empty(t, cache.capturedBuckets())
	require.Empty(t, repo.calls)
	require.Empty(t, seen, "credential refresh must not suppress later membership work in the same batch")

	err = svc.handleOutboxEvent(context.Background(), SchedulerOutboxEvent{
		EventType: SchedulerOutboxEventAccountChanged, AccountID: &repo.account.ID,
	}, seen)
	require.NoError(t, err)
	require.ElementsMatch(t, schedulerBucketsForTest([]int64{15}, PlatformOpenAI), cache.capturedBuckets())
}

func TestSchedulerCredentialEventHintIsStrictAndCannotSkipGroupChanges(t *testing.T) {
	for _, tc := range []struct {
		name      string
		eventType string
		payload   map[string]any
	}{
		{name: "legacy nil", eventType: SchedulerOutboxEventAccountChanged},
		{name: "unknown hint", eventType: SchedulerOutboxEventAccountChanged, payload: map[string]any{"future_hint": true}},
		{name: "false hint", eventType: SchedulerOutboxEventAccountChanged, payload: map[string]any{"cache_only": false}},
		{name: "string hint", eventType: SchedulerOutboxEventAccountChanged, payload: map[string]any{"cache_only": "true"}},
		{name: "numeric hint", eventType: SchedulerOutboxEventAccountChanged, payload: map[string]any{"cache_only": 1}},
		{name: "group event", eventType: SchedulerOutboxEventAccountGroupsChanged, payload: map[string]any{"cache_only": true}},
		{name: "group payload", eventType: SchedulerOutboxEventAccountChanged, payload: map[string]any{"cache_only": true, "group_ids": []any{int64(15)}}},
		{name: "empty group payload", eventType: SchedulerOutboxEventAccountChanged, payload: map[string]any{"cache_only": true, "group_ids": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, cache, repo := newCredentialsEventService()
			err := svc.handleOutboxEvent(context.Background(), SchedulerOutboxEvent{
				EventType: tc.eventType, AccountID: &repo.account.ID, Payload: tc.payload,
			}, make(map[batchSeenKey]struct{}))
			require.NoError(t, err)
			require.ElementsMatch(t, schedulerBucketsForTest([]int64{15}, PlatformOpenAI), cache.capturedBuckets())
		})
	}
}

func TestSchedulerCredentialEventFailureRemainsRetryable(t *testing.T) {
	for _, failCache := range []bool{false, true} {
		svc, cache, repo := newCredentialsEventService()
		wantErr := errors.New("temporarily unavailable")
		if failCache {
			cache.setErr = wantErr
		} else {
			repo.getErr = wantErr
		}
		event := SchedulerOutboxEvent{
			EventType: SchedulerOutboxEventAccountChanged, AccountID: &repo.account.ID,
			Payload: map[string]any{"cache_only": true},
		}
		require.ErrorIs(t, svc.handleOutboxEvent(context.Background(), event, nil), wantErr)
		require.Empty(t, cache.capturedBuckets())
		cache.setErr, repo.getErr = nil, nil
		require.NoError(t, svc.handleOutboxEvent(context.Background(), event, nil))
		require.Equal(t, repo.account, cache.account)
	}
}

func TestSchedulerCredentialEventDoesNotHideDeletedAccount(t *testing.T) {
	svc, cache, repo := newCredentialsEventService()
	repo.getErr = ErrAccountNotFound
	err := svc.handleOutboxEvent(context.Background(), SchedulerOutboxEvent{
		EventType: SchedulerOutboxEventAccountChanged, AccountID: &repo.account.ID,
		Payload: map[string]any{"cache_only": true},
	}, nil)
	require.NoError(t, err)
	set, deleted := cache.accountWrites()
	require.Empty(t, set)
	require.Equal(t, []int64{repo.account.ID}, deleted)
}

func TestSchedulerCredentialEventLegacyHandlerMayIgnoreHint(t *testing.T) {
	svc, cache, repo := newCredentialsEventService()
	// The previous consumer always called the membership handler. An additive
	// payload hint keeps that path valid during a rolling upgrade.
	err := svc.handleAccountEvent(context.Background(), &repo.account.ID,
		map[string]any{"cache_only": true}, nil, false)
	require.NoError(t, err)
	require.Equal(t, repo.account, cache.account)
	require.ElementsMatch(t, schedulerBucketsForTest([]int64{15}, PlatformOpenAI), cache.capturedBuckets())
}
