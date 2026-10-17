package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestWebImageProbeLockAcrossInstances(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "busy", true: "expired owner"}[expire], func(t *testing.T) {
			mr := miniredis.RunT(t)
			account := webImageCapacityAccount(42)
			repo := &webImgAccountRepo{accounts: map[int64]*Account{42: account}}
			first := NewOpenAIWebImagesService(webImgTestCfg("redis"), newWebImageTestRedisClient(t, mr.Addr()), repo)
			second := NewOpenAIWebImagesService(webImgTestCfg("redis"), newWebImageTestRedisClient(t, mr.Addr()), repo)
			entered := make(chan int, 2)
			release := []chan struct{}{make(chan struct{}), make(chan struct{})}
			var calls atomic.Int32
			factory := func(string) (*req.Client, error) {
				client := req.C()
				client.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
					return func(r *http.Request) (*http.Response, error) {
						i := int(calls.Add(1)) - 1
						entered <- i
						select {
						case <-release[i]:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"limits_progress":[{"feature_name":"image_gen","remaining":3}]}`))}, nil
					}
				})
				return client, nil
			}
			first.Driver().ClientFactory = factory
			second.Driver().ClientFactory = factory
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 2)
			go func() { _, err := first.ProbeAccount(ctx, 42, true); done <- err }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("first probe did not start")
			}
			key := first.prefix() + "probe:lock:42"
			owner, err := mr.Get(key)
			require.NoError(t, err)
			if !expire {
				_, err = second.ProbeAccount(ctx, 42, true)
				require.ErrorContains(t, err, "probe already in progress")
				current, err := mr.Get(key)
				require.NoError(t, err)
				require.Equal(t, owner, current)
				require.EqualValues(t, 1, calls.Load())
				close(release[0])
				require.NoError(t, <-done)
				require.False(t, mr.Exists(key))
				return
			}
			mr.FastForward(46 * time.Second)
			go func() { _, err := second.ProbeAccount(ctx, 42, true); done <- err }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("replacement probe did not start")
			}
			newOwner, err := mr.Get(key)
			require.NoError(t, err)
			require.NotEqual(t, owner, newOwner)
			close(release[0])
			require.NoError(t, <-done)
			current, err := mr.Get(key)
			require.NoError(t, err)
			require.Equal(t, newOwner, current, "expired owner must not delete a replacement lock")
			close(release[1])
			require.NoError(t, <-done)
			require.False(t, mr.Exists(key))
		})
	}
}

func TestWebImageCanceledProbeReleasesLock(t *testing.T) {
	mr := miniredis.RunT(t)
	account := webImageCapacityAccount(42)
	web := NewOpenAIWebImagesService(webImgTestCfg("redis"), newWebImageTestRedisClient(t, mr.Addr()), &webImgAccountRepo{accounts: map[int64]*Account{42: account}})
	web.Driver().ClientFactory = func(string) (*req.Client, error) {
		client := req.C()
		client.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
			return func(r *http.Request) (*http.Response, error) {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
		})
		return client, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := web.ProbeAccount(ctx, 42, true)
	require.Error(t, err)
	require.False(t, mr.Exists(web.prefix()+"probe:lock:42"))
}
