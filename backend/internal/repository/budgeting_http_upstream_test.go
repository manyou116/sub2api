package repository

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type budgetingHTTPUpstreamStub struct {
	calls atomic.Int64
	resp  *http.Response
}

func (s *budgetingHTTPUpstreamStub) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	s.calls.Add(1)
	return s.resp, nil
}

func (s *budgetingHTTPUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, concurrency)
}

func TestBudgetingHTTPUpstreamDenyDoesNotSend(t *testing.T) {
	budget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/reserve", r.URL.Path)
		_, _ = w.Write([]byte(`{"allowed":false,"managed":true,"mode":"enforce","reason":"daily cap"}`))
	}))
	defer budget.Close()
	t.Setenv("PROXY_BUDGET_URL", budget.URL)
	t.Setenv("PROXY_BUDGET_TOKEN", "test-token")

	next := &budgetingHTTPUpstreamStub{}
	upstream := newBudgetingHTTPUpstream(next)
	request, err := http.NewRequest(http.MethodGet, "https://upstream.example", nil)
	require.NoError(t, err)
	_, err = upstream.Do(request, "http://proxy.example:8080", 1, 1)
	require.Error(t, err)
	require.Zero(t, next.calls.Load())
}

func TestBudgetingHTTPUpstreamStreamRenewsAndSettlesOnce(t *testing.T) {
	var reserves atomic.Int64
	var settles atomic.Int64
	budget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reserve":
			reserves.Add(1)
			_, _ = w.Write([]byte(`{"allowed":true,"managed":true,"mode":"enforce","granted_bytes":69632}`))
		case "/settle":
			settles.Add(1)
			_, _ = w.Write([]byte(`{"settled":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer budget.Close()
	t.Setenv("PROXY_BUDGET_URL", budget.URL)
	t.Setenv("PROXY_BUDGET_TOKEN", "test-token")

	next := &budgetingHTTPUpstreamStub{resp: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), 130<<10)))}}
	upstream := newBudgetingHTTPUpstream(next)
	request, err := http.NewRequest(http.MethodGet, "https://upstream.example", nil)
	require.NoError(t, err)
	response, err := upstream.Do(request, "http://proxy.example:8080", 1, 1)
	require.NoError(t, err)
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.GreaterOrEqual(t, reserves.Load(), int64(2))
	require.Equal(t, int64(1), settles.Load())
}

func TestBudgetingHTTPUpstreamPartialConfigurationFailsClosed(t *testing.T) {
	t.Setenv("PROXY_BUDGET_URL", "http://127.0.0.1:1234")
	t.Setenv("PROXY_BUDGET_TOKEN", "")
	next := &budgetingHTTPUpstreamStub{}
	upstream := newBudgetingHTTPUpstream(next)
	request, err := http.NewRequest(http.MethodGet, "https://upstream.example", nil)
	require.NoError(t, err)
	_, err = upstream.Do(request, "http://proxy.example:8080", 1, 1)
	require.Error(t, err)
	require.Zero(t, next.calls.Load())
}
