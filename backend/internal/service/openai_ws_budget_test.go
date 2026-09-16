package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type budgetWSConnStub struct {
	readPayload []byte
	writes      atomic.Int64
	closed      atomic.Int64
}

func (c *budgetWSConnStub) WriteJSON(_ context.Context, _ any) error    { c.writes.Add(1); return nil }
func (c *budgetWSConnStub) ReadMessage(context.Context) ([]byte, error) { return c.readPayload, nil }
func (c *budgetWSConnStub) Ping(context.Context) error                  { return nil }
func (c *budgetWSConnStub) Close() error                                { c.closed.Add(1); return nil }

type budgetWSDialerStub struct {
	conn  openAIWSClientConn
	calls atomic.Int64
}

func (d *budgetWSDialerStub) Dial(_ context.Context, _ string, _ http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	d.calls.Add(1)
	return d.conn, 0, nil, nil
}

func TestBudgetingOpenAIWSDialerEnforceAdmitsWriteAndBoundedRead(t *testing.T) {
	var reserves atomic.Int64
	var settles atomic.Int64
	budget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/reserve" {
			if reserves.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"allowed":true,"managed":true,"mode":"enforce","granted_bytes":69632}`))
				return
			}
			_, _ = w.Write([]byte(`{"allowed":true,"managed":true,"mode":"enforce","granted_bytes":33554432}`))
			return
		}
		settles.Add(1)
		_, _ = w.Write([]byte(`{"settled":true}`))
	}))
	defer budget.Close()
	t.Setenv("PROXY_BUDGET_URL", budget.URL)
	t.Setenv("PROXY_BUDGET_TOKEN", "test-token")

	baseConn := &budgetWSConnStub{readPayload: []byte("reply")}
	next := &budgetWSDialerStub{conn: baseConn}
	dialer := newBudgetingOpenAIWSClientDialer(next)
	conn, _, _, err := dialer.Dial(context.Background(), "wss://upstream.example", nil, "http://proxy.example:8080")
	require.NoError(t, err)
	require.NoError(t, conn.WriteJSON(context.Background(), map[string]string{"type": "ping"}))
	payload, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)
	require.Equal(t, []byte("reply"), payload)
	require.NoError(t, conn.Close())
	require.Equal(t, int64(1), next.calls.Load())
	require.GreaterOrEqual(t, reserves.Load(), int64(2), "read reserves before exposing one bounded message")
	require.Equal(t, int64(1), settles.Load())
}

func TestBudgetingOpenAIWSRejectsBeforeWriteAndCloses(t *testing.T) {
	budget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/reserve" {
			_, _ = w.Write([]byte(`{"allowed":false,"managed":true,"mode":"enforce","reason":"daily cap"}`))
			return
		}
		_, _ = w.Write([]byte(`{"settled":true}`))
	}))
	defer budget.Close()
	t.Setenv("PROXY_BUDGET_URL", budget.URL)
	t.Setenv("PROXY_BUDGET_TOKEN", "test-token")

	next := &budgetWSDialerStub{conn: &budgetWSConnStub{}}
	dialer := newBudgetingOpenAIWSClientDialer(next)
	_, _, _, err := dialer.Dial(context.Background(), "wss://upstream.example", nil, "http://proxy.example:8080")
	require.Error(t, err)
	require.Zero(t, next.calls.Load())
}
