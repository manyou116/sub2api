//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type webImageBatchRepo struct {
	*webImageTestRepo
	reads, bulkReads, stateReads int
	observed                     map[int64]WebImageCooldown
}

func (r *webImageBatchRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.reads++
	return r.webImageTestRepo.GetByID(ctx, id)
}
func (r *webImageBatchRepo) GetByIDs(ctx context.Context, ids []int64) ([]*Account, error) {
	r.bulkReads++
	return r.webImageTestRepo.GetByIDs(ctx, ids)
}
func (r *webImageBatchRepo) WebImageCooldowns(_ context.Context, ids []int64) (map[int64]WebImageCooldown, error) {
	r.stateReads++
	return r.observed, nil
}
func (r *webImageBatchRepo) ClearWebImageCooldownIfObserved(_ context.Context, id int64, old WebImageCooldown) error {
	if r.observed[id].Until == old.Until {
		delete(r.observed, id)
	}
	return nil
}

func TestWebImageCandidateStateUsesBatchesAndExecutionRechecks(t *testing.T) {
	s, repo, parent, shadow, _ := webImageFixture(t)
	batch := &webImageBatchRepo{webImageTestRepo: repo, observed: map[int64]WebImageCooldown{}}
	s.accounts = batch
	s.gateway.accountRepo = batch
	ctx := WithOpenAIImageGenerationIntent(context.Background())
	candidateCtx := s.gateway.webImageCandidateContext(ctx, []Account{*shadow})
	for range 5 {
		_, err := s.eligible(candidateCtx, shadow)
		require.NoError(t, err)
	}
	require.Equal(t, 1, batch.bulkReads)
	require.Equal(t, 1, batch.stateReads)
	require.Zero(t, batch.reads)
	until := time.Now().Add(time.Hour)
	batch.observed[parent.ID] = WebImageCooldown{Until: &until, Reason: "quota_exhausted"}
	_, err := s.eligible(ctx, shadow)
	require.ErrorContains(t, err, "quota is exhausted")
	require.Equal(t, 1, batch.reads)
}

func TestWebImageBatchStatusIncludesPerItemErrors(t *testing.T) {
	s, repo, _, shadow, _ := webImageFixture(t)
	batch := &webImageBatchRepo{webImageTestRepo: repo}
	s.accounts = batch
	result, err := s.BatchStatus(context.Background(), []int64{shadow.ID, 1, 999})
	require.NoError(t, err)
	require.Len(t, result.Statuses, 1)
	require.Len(t, result.Errors, 2)
	require.Equal(t, 1, batch.bulkReads)
	require.Equal(t, 1, batch.stateReads)
	_, err = s.BatchStatus(context.Background(), []int64{-1})
	require.Error(t, err)
}
