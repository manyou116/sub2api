package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func webImageCapacityAccount(id int64) *Account {
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 10,
		Credentials: map[string]any{"access_token": "test-token"},
		Extra:       map[string]any{"openai_web_images": map[string]any{"enabled": true, "max_inflight": 1}},
	}
}

// Both service instances use separate Redis connections to the same slot key.
func testWebImageRedisConcurrentAdmission(t *testing.T, addr string) {
	t.Helper()
	services := make([]*OpenAIWebImagesService, 2)
	for i := range services {
		client := newWebImageTestRedisClient(t, addr)
		services[i] = NewOpenAIWebImagesService(webImgTestCfg("redis"), client, nil)
	}
	type attempt struct {
		id  string
		svc *OpenAIWebImagesService
		ok  bool
		err error
	}
	start := make(chan struct{})
	results := make(chan attempt, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			a := attempt{id: fmt.Sprintf("request-%d", i), svc: services[i%2]}
			a.ok, a.err = a.svc.Acquire(context.Background(), 42, 1, a.id)
			results <- a
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	var winners []attempt
	for a := range results {
		require.NoError(t, a.err)
		if a.ok {
			winners = append(winners, a)
		}
	}
	require.Len(t, winners, 1)
	require.True(t, services[1].IsInflightFull(context.Background(), webImageCapacityAccount(42)))
	winner := winners[0]
	ok, err := winner.svc.Acquire(context.Background(), 42, 1, winner.id)
	require.NoError(t, err)
	require.True(t, ok, "repeating the same request must not increment its lease")
	winner.svc.Release(context.Background(), 42, winner.id)
	ok, err = services[1].Acquire(context.Background(), 42, 1, "next")
	require.NoError(t, err)
	require.True(t, ok)
	winner.svc.Release(context.Background(), 42, winner.id)
	require.True(t, services[0].IsInflightFull(context.Background(), webImageCapacityAccount(42)), "duplicate release must not free next request")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	services[1].Release(ctx, 42, "next")
	require.False(t, services[0].IsInflightFull(context.Background(), webImageCapacityAccount(42)))
}

func TestWebImageRedisConcurrentAdmission(t *testing.T) {
	testWebImageRedisConcurrentAdmission(t, miniredis.RunT(t).Addr())
}

func TestWebImageRedisExpiredOwnerCannotReleaseNewLease(t *testing.T) {
	mr := miniredis.RunT(t)
	client := newWebImageTestRedisClient(t, mr.Addr())
	svc := NewOpenAIWebImagesService(webImgTestCfg("redis"), client, nil)
	ok, err := svc.Acquire(context.Background(), 42, 1, "expired")
	require.NoError(t, err)
	require.True(t, ok)
	mr.FastForward(61 * time.Second)
	ok, err = svc.Acquire(context.Background(), 42, 1, "new")
	require.NoError(t, err)
	require.True(t, ok)
	svc.Release(context.Background(), 42, "expired")
	require.True(t, svc.IsInflightFull(context.Background(), webImageCapacityAccount(42)))
}

func TestWebImageCapacityBusyDoesNotProbeOrFail(t *testing.T) {
	account := webImageCapacityAccount(42)
	svc := NewOpenAIWebImagesService(webImgTestCfg("memory"), nil, nil)
	ok, err := svc.Acquire(context.Background(), 42, 1, "busy")
	require.NoError(t, err)
	require.True(t, ok)
	svc.Driver().ClientFactory = func(string) (*req.Client, error) { t.Fatal("busy account must not contact upstream"); return nil, nil }
	gw := &OpenAIGatewayService{webImages: svc}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	_, err = gw.forwardOpenAIImagesLegacyWeb(context.Background(), c, account, &OpenAIImagesRequest{Model: "gpt-image-2"}, "")
	var capacity *OpenAIWebImagesCapacityError
	require.ErrorAs(t, err, &capacity)
	require.Equal(t, "inflight_full", capacity.Reason)
	require.Zero(t, svc.ParseAccountConfig(account).Stats.Fail)
	require.Nil(t, account.WebImageRateLimitResetAt)
}

func TestWebImageCancelDuringProbeReleasesReservedSlot(t *testing.T) {
	mr := miniredis.RunT(t)
	client := newWebImageTestRedisClient(t, mr.Addr())
	account := webImageCapacityAccount(42)
	repo := &webImgAccountRepo{accounts: map[int64]*Account{42: account}}
	cfg := webImgTestCfg("redis")
	cfg.Gateway.OpenAIWebImages.UnknownQuotaPolicy = "optimistic"
	svc := NewOpenAIWebImagesService(cfg, client, repo)
	entered := make(chan struct{})
	var once sync.Once
	svc.Driver().ClientFactory = func(string) (*req.Client, error) {
		c := req.C()
		c.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
			return func(r *http.Request) (*http.Response, error) {
				once.Do(func() { close(entered) })
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
		})
		return c, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gw := &OpenAIGatewayService{webImages: svc}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	done := make(chan error, 1)
	go func() {
		_, err := gw.forwardOpenAIImagesLegacyWeb(ctx, c, account, &OpenAIImagesRequest{Model: "gpt-image-2"}, "")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not start")
	}
	require.True(t, svc.IsInflightFull(context.Background(), account), "probe must own the image slot")
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not finish")
	}
	require.False(t, svc.IsInflightFull(context.Background(), account))
	require.False(t, svc.IsWebRateLimited(context.Background(), account.ID), "cancellation must not penalize the account")
}

func TestWebImageTransientFailureBackoffAndRecovery(t *testing.T) {
	mr := miniredis.RunT(t)
	client := newWebImageTestRedisClient(t, mr.Addr())
	account := webImageCapacityAccount(42)
	repo := &webImgAccountRepo{accounts: map[int64]*Account{42: account}}
	svc := NewOpenAIWebImagesService(webImgTestCfg("redis"), client, repo)
	svc.setQuotaCache(context.Background(), 42, webImageQuotaCache{Remaining: 3, ProbedAt: time.Now()})
	svc.Driver().ClientFactory = func(string) (*req.Client, error) {
		c := req.C()
		c.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
			return func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 502, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("upstream unavailable"))}, nil
			}
		})
		return c, nil
	}
	gw := &OpenAIGatewayService{webImages: svc}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	_, err := gw.forwardOpenAIImagesLegacyWeb(context.Background(), c, account, &OpenAIImagesRequest{Model: "gpt-image-2"}, "")
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.True(t, svc.IsWebRateLimited(context.Background(), 42))
	require.True(t, gw.isOpenAIAccountRuntimeBlockedForRequest(context.Background(), account, OpenAIImagesCapabilityBasic, "gpt-image-2"))
	require.False(t, gw.isOpenAIAccountRuntimeBlockedForRequest(context.Background(), account, "", "gpt-5.4"), "web backoff must not block text")
	require.False(t, svc.IsInflightFull(context.Background(), account))
	require.Nil(t, account.WebImageRateLimitResetAt)
	q, known := svc.getQuotaCache(context.Background(), 42)
	require.True(t, known)
	require.Equal(t, 3, q.Remaining)
	mr.FastForward(31 * time.Second)
	require.False(t, svc.IsWebRateLimited(context.Background(), 42), "expiry permits recovery")
	require.False(t, gw.isOpenAIAccountRuntimeBlockedForRequest(context.Background(), account, OpenAIImagesCapabilityBasic, "gpt-image-2"))
}

func TestWebImageDeadlineReleasesSlot(t *testing.T) {
	mr := miniredis.RunT(t)
	account := webImageCapacityAccount(42)
	repo := &webImgAccountRepo{accounts: map[int64]*Account{42: account}}
	svc := NewOpenAIWebImagesService(webImgTestCfg("redis"), newWebImageTestRedisClient(t, mr.Addr()), repo)
	svc.setQuotaCache(context.Background(), 42, webImageQuotaCache{Remaining: 3, ProbedAt: time.Now()})
	svc.Driver().ClientFactory = func(string) (*req.Client, error) {
		c := req.C()
		c.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
			return func(r *http.Request) (*http.Response, error) {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
		})
		return c, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	gw := &OpenAIGatewayService{webImages: svc}
	_, err := gw.forwardOpenAIImagesLegacyWeb(ctx, c, account, &OpenAIImagesRequest{Model: "gpt-image-2"}, "")
	require.Error(t, err)
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	require.False(t, svc.IsInflightFull(context.Background(), account))
	require.False(t, svc.IsWebRateLimited(context.Background(), 42))
}

func TestWebImageReservedSlotDoesNotBypassQuota(t *testing.T) {
	account := webImageCapacityAccount(42)
	repo := &webImgAccountRepo{accounts: map[int64]*Account{42: account}}
	svc := NewOpenAIWebImagesService(webImgTestCfg("memory"), nil, repo)
	svc.setQuotaCache(context.Background(), 42, webImageQuotaCache{Remaining: 0, ProbedAt: time.Now()})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	gw := &OpenAIGatewayService{webImages: svc}
	_, err := gw.forwardOpenAIImagesLegacyWeb(context.Background(), c, account, &OpenAIImagesRequest{Model: "gpt-image-2"}, "")
	var capacity *OpenAIWebImagesCapacityError
	require.ErrorAs(t, err, &capacity)
	require.Equal(t, "no_quota", capacity.Reason)
	require.False(t, svc.IsInflightFull(context.Background(), account))
}
