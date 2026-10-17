package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestWebImageProbeKeepsUnknownQuota(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		for _, policy := range []string{"optimistic", "strict"} {
			t.Run(backend+"/"+policy, func(t *testing.T) {
				account := webImageCapacityAccount(42)
				cfg := webImgTestCfg(backend)
				cfg.Gateway.OpenAIWebImages.UnknownQuotaPolicy = policy
				web := NewOpenAIWebImagesService(cfg, nil, &webImgAccountRepo{accounts: map[int64]*Account{42: account}})
				if backend == "redis" {
					web.rdb = newWebImageTestRedisClient(t, miniredis.RunT(t).Addr())
				}
				calls := 0
				web.Driver().ClientFactory = func(string) (*req.Client, error) {
					client := req.C()
					client.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
						return func(*http.Request) (*http.Response, error) {
							calls++
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"limits_progress":[]}`))}, nil
						}
					})
					return client, nil
				}
				for i := 0; i < 2; i++ {
					st, err := web.ProbeAccount(context.Background(), 42, false)
					require.NoError(t, err)
					require.False(t, st.QuotaKnown)
					require.Nil(t, st.Remaining)
					require.Equal(t, policy == "optimistic", st.Schedulable)
					require.Nil(t, account.WebImageRateLimitResetAt)
				}
				require.Equal(t, 1, calls, "cache unknown results to avoid repeated probes")
				web.setQuotaCache(context.Background(), 42, webImageQuotaCache{Remaining: 3, ResetAt: nil, ProbedAt: time.Now()})
				q, known := web.getQuotaCache(context.Background(), 42)
				require.True(t, known)
				require.Nil(t, q.ResetAt)
			})
		}
	}
}
