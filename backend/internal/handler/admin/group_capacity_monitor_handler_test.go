package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type capacityMonitorHandlerRepo struct {
	calls int
	err   error
}

func (r *capacityMonitorHandlerRepo) ReadSnapshot(context.Context, int64, time.Duration) (*service.GroupCapacityMonitorSnapshot, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return &service.GroupCapacityMonitorSnapshot{AsOf: time.Now().UTC(), GroupName: "test"}, nil
}

func TestCapacityMonitorHandlerMapsRepositoryErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		err    error
		status int
	}{{service.ErrCapacityMonitorNotFound, 404}, {service.ErrCapacityMonitorBudget, 422}, {context.DeadlineExceeded, 503}} {
		repo := &capacityMonitorHandlerRepo{err: test.err}
		cfg := &config.Config{}
		h := NewGroupHandlerWithCapacityMonitor(nil, nil, nil, cfg, service.NewGroupCapacityMonitorService(repo))
		r := gin.New()
		r.GET("/groups/:id/capacity-monitor", h.GetCapacityMonitor)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/groups/24/capacity-monitor", nil))
		require.Equal(t, test.status, w.Code)
	}
}

type capacityMonitorConcurrentRepo struct {
	calls   atomic.Int32
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (r *capacityMonitorConcurrentRepo) ReadSnapshot(context.Context, int64, time.Duration) (*service.GroupCapacityMonitorSnapshot, error) {
	r.calls.Add(1)
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return &service.GroupCapacityMonitorSnapshot{AsOf: time.Now().UTC()}, nil
}

func TestCapacityMonitorHandlerConcurrentMissesCoalesce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &capacityMonitorConcurrentRepo{release: make(chan struct{}), entered: make(chan struct{})}
	cfg := &config.Config{}
	h := NewGroupHandlerWithCapacityMonitor(nil, nil, nil, cfg, service.NewGroupCapacityMonitorService(repo))
	r := gin.New()
	r.GET("/groups/:id/capacity-monitor", h.GetCapacityMonitor)
	var wg sync.WaitGroup
	statuses := make(chan int, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/groups/24/capacity-monitor", nil))
			statuses <- w.Code
		}()
	}
	<-repo.entered
	close(repo.release)
	wg.Wait()
	close(statuses)
	for status := range statuses {
		require.Equal(t, 200, status)
	}
	require.Equal(t, int32(1), repo.calls.Load())
}

func TestCapacityMonitorHandlerComputesOnlyOnValidRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &capacityMonitorHandlerRepo{}
	cfg := &config.Config{}
	svc := service.NewGroupCapacityMonitorService(repo)
	h := NewGroupHandlerWithCapacityMonitor(nil, nil, nil, cfg, svc)
	r := gin.New()
	r.GET("/groups/:id/capacity-monitor", h.GetCapacityMonitor)
	require.Zero(t, repo.calls)
	for _, url := range []string{"/groups/x/capacity-monitor", "/groups/24/capacity-monitor?history=100d", "/groups/24/capacity-monitor?horizon=bad"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
		require.Equal(t, 400, w.Code)
	}
	require.Zero(t, repo.calls)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/groups/24/capacity-monitor", nil))
	require.Equal(t, 200, w.Code)
	require.Equal(t, 1, repo.calls)
}

func TestCapacityMonitorHandlerSnapshotCacheAndETag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &capacityMonitorHandlerRepo{}
	cfg := &config.Config{}
	h := NewGroupHandlerWithCapacityMonitor(nil, nil, nil, cfg, service.NewGroupCapacityMonitorService(repo))
	r := gin.New()
	r.GET("/groups/:id/capacity-monitor", h.GetCapacityMonitor)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/groups/24/capacity-monitor", nil))
	require.Equal(t, 200, w.Code)
	require.Equal(t, "miss", w.Header().Get("X-Snapshot-Cache"))
	require.NotEmpty(t, w.Header().Get("ETag"))
	request := httptest.NewRequest(http.MethodGet, "/groups/24/capacity-monitor", nil)
	request.Header.Set("If-None-Match", w.Header().Get("ETag"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, request)
	require.Equal(t, 304, w.Code)
	require.Equal(t, 1, repo.calls)
}
