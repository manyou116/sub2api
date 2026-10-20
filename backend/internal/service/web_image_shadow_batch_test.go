//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type batchWebImageCreatorFunc func(context.Context, int64, WebImageShadowOptions) (*Account, error)

func (f batchWebImageCreatorFunc) CreateWebImageShadow(ctx context.Context, id int64, opts WebImageShadowOptions) (*Account, error) {
	return f(ctx, id, opts)
}

func TestWebImageShadowBatchPartialResults(t *testing.T) {
	concurrency := 2
	var calls atomic.Int32
	creator := batchWebImageCreatorFunc(func(_ context.Context, id int64, opts WebImageShadowOptions) (*Account, error) {
		calls.Add(1)
		require.Equal(t, &concurrency, opts.Concurrency)
		require.Nil(t, opts.Priority, "each parent's priority is inherited")
		require.Nil(t, opts.GroupIDs, "each parent's groups are inherited")
		switch id {
		case 20:
			return nil, infraerrors.New(http.StatusConflict, "WEB_IMAGE_SHADOW_ALREADY_EXISTS", "already exists")
		case 30:
			return nil, infraerrors.BadRequest("WEB_IMAGE_SHADOW_INVALID_PARENT", "unsupported parent")
		case 40:
			return nil, ErrAccountNotFound
		case 50:
			return nil, errors.New("database failed with private details")
		default:
			return &Account{ID: id + 100}, nil
		}
	})
	result, err := CreateWebImageShadows(context.Background(), creator, WebImageShadowBatchInput{ParentIDs: []int64{10, 20, 30, 10, 40, 50, 60}, Concurrency: &concurrency})
	require.NoError(t, err)
	require.EqualValues(t, 6, calls.Load())
	require.Equal(t, 2, result.Created)
	require.Equal(t, 1, result.Existing)
	require.Equal(t, 2, result.Skipped)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, []WebImageShadowBatchItem{
		{ParentID: 10, ShadowID: 110, Status: "created"},
		{ParentID: 20, Status: "existing", Code: "WEB_IMAGE_SHADOW_ALREADY_EXISTS", Message: "already exists"},
		{ParentID: 30, Status: "skipped", Code: "WEB_IMAGE_SHADOW_INVALID_PARENT", Message: "unsupported parent"},
		{ParentID: 40, Status: "skipped", Code: "ACCOUNT_NOT_FOUND", Message: "account not found"},
		{ParentID: 50, Status: "failed", Code: "WEB_IMAGE_SHADOW_CREATE_FAILED", Message: "could not create Web image shadow"},
		{ParentID: 60, ShadowID: 160, Status: "created"},
	}, result.Results)
}

func TestWebImageShadowBatchValidatesBeforeMutating(t *testing.T) {
	zero := 0
	for _, input := range []WebImageShadowBatchInput{
		{}, {ParentIDs: make([]int64, MaxWebImageShadowBatch+1)},
		{ParentIDs: []int64{1, 0}}, {ParentIDs: []int64{1, -1}},
		{ParentIDs: []int64{1}, Concurrency: &zero},
	} {
		_, err := CreateWebImageShadows(context.Background(), batchWebImageCreatorFunc(func(context.Context, int64, WebImageShadowOptions) (*Account, error) {
			t.Error("invalid batch must not start creation")
			return nil, nil
		}), input)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	}
}

func TestWebImageShadowBatchBoundsWorkersAndStopsOnCancellation(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded", true: "canceled"}[cancelEarly], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started, release := make(chan struct{}, 20), make(chan struct{})
			var calls atomic.Int32
			creator := batchWebImageCreatorFunc(func(ctx context.Context, id int64, _ WebImageShadowOptions) (*Account, error) {
				calls.Add(1)
				started <- struct{}{}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-release:
					return &Account{ID: id + 100}, nil
				}
			})
			resultCh := make(chan *WebImageShadowBatchResult, 1)
			go func() {
				result, _ := CreateWebImageShadows(ctx, creator, WebImageShadowBatchInput{ParentIDs: []int64{1, 2, 3, 4, 5, 6, 7, 8}})
				resultCh <- result
			}()
			for range 4 {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("workers did not start")
				}
			}
			require.EqualValues(t, 4, calls.Load(), "batch must not create more than four accounts concurrently")
			if cancelEarly {
				cancel()
			} else {
				close(release)
			}
			result := <-resultCh
			require.Len(t, result.Results, 8)
			if cancelEarly {
				require.EqualValues(t, 4, calls.Load(), "cancellation must not start queued accounts")
				require.Equal(t, 8, result.Failed)
				for _, item := range result.Results {
					require.Equal(t, "WEB_IMAGE_SHADOW_BATCH_CANCELED", item.Code)
				}
			} else {
				require.Equal(t, 8, result.Created)
			}
		})
	}
}

func TestWebImageShadowBatchConcurrentRetryKeepsExistingChild(t *testing.T) {
	var mu sync.Mutex
	var created bool
	creator := batchWebImageCreatorFunc(func(context.Context, int64, WebImageShadowOptions) (*Account, error) {
		mu.Lock()
		defer mu.Unlock()
		if created {
			return nil, infraerrors.New(http.StatusConflict, "WEB_IMAGE_SHADOW_ALREADY_EXISTS", "already exists")
		}
		created = true
		return &Account{ID: 20, Status: "inactive", Schedulable: false}, nil
	})
	results := make(chan *WebImageShadowBatchResult, 2)
	for range 2 {
		go func() {
			r, _ := CreateWebImageShadows(context.Background(), creator, WebImageShadowBatchInput{ParentIDs: []int64{10}})
			results <- r
		}()
	}
	a, b := <-results, <-results
	require.Equal(t, 1, a.Created+b.Created)
	require.Equal(t, 1, a.Existing+b.Existing)
	require.Zero(t, a.Failed+b.Failed)
}

func TestWebImageShadowBatchReusesLifecycleWithoutResumingExistingChild(t *testing.T) {
	ctx := context.Background()
	svc, repo, parent := webShadowTestParent(t)
	result, err := CreateWebImageShadows(ctx, svc, WebImageShadowBatchInput{ParentIDs: []int64{parent.ID, parent.ID}})
	require.NoError(t, err)
	require.Equal(t, 1, result.Created)
	child := repo.accounts[result.Results[0].ShadowID]
	require.Equal(t, parent.Priority, child.Priority)
	require.Equal(t, parent.GroupIDs, repo.groupsOf[child.ID], "stub stores group bindings separately from account rows")
	require.Equal(t, 1, child.Concurrency)
	child.Schedulable, child.Concurrency = false, 7
	require.NoError(t, repo.Update(ctx, child))
	requestedConcurrency := 3
	result, err = CreateWebImageShadows(ctx, svc, WebImageShadowBatchInput{ParentIDs: []int64{parent.ID}, Concurrency: &requestedConcurrency})
	require.NoError(t, err)
	require.Equal(t, 1, result.Existing)
	require.Equal(t, 7, repo.accounts[child.ID].Concurrency)
	require.False(t, repo.accounts[child.ID].Schedulable)
}
