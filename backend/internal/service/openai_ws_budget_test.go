package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxybudget"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

type budgetWSConnStub struct {
	readPayload []byte
	writes      atomic.Int64
	closed      atomic.Int64
}

func (*budgetWSConnStub) SupportsIdlePingWithoutReader() bool { return false }

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

func TestBudgetingOpenAIWSForwardsIdlePingCapability(t *testing.T) {
	wrapped := &budgetingOpenAIWSClientConn{next: &budgetWSConnStub{}}
	require.False(t, wrapped.SupportsIdlePingWithoutReader())
}

type budgetFrameConn struct {
	budgetWSConnStub
	readPayload []byte
	readCalls   atomic.Int64
	writeCalls  atomic.Int64
}

func (c *budgetFrameConn) ReadFrameBudgeted(ctx context.Context, reserve func(context.Context, int64) (*proxybudget.IOReservation, error)) (coderws.MessageType, []byte, error) {
	reservation, err := reserve(ctx, int64(len(c.readPayload)))
	if err != nil {
		return coderws.MessageText, nil, err
	}
	reservation.Finish(int64(len(c.readPayload)))
	c.readCalls.Add(1)
	return coderws.MessageBinary, c.readPayload, nil
}

func (c *budgetFrameConn) WriteFrame(_ context.Context, _ coderws.MessageType, _ []byte) error {
	c.writeCalls.Add(1)
	return nil
}

func TestBudgetingOpenAIWSRemainsFrameConnAndAccountsBothDirections(t *testing.T) {
	var reserves atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/reserve" {
			reserves.Add(1)
			_, _ = w.Write([]byte(`{"allowed":true,"managed":true,"mode":"enforce","granted_bytes":262144}`))
			return
		}
		_, _ = w.Write([]byte(`{"settled":true}`))
	}))
	defer server.Close()
	lease, err := proxybudget.New(server.URL, "test", server.Client()).OpenLease(context.Background(), "http://proxy.example:8080")
	require.NoError(t, err)
	inner := &budgetFrameConn{readPayload: []byte("upstream")}
	wrapped := &budgetingOpenAIWSClientConn{next: inner, lease: lease}
	var _ interface {
		ReadFrame(context.Context) (coderws.MessageType, []byte, error)
		WriteFrame(context.Context, coderws.MessageType, []byte) error
	} = wrapped
	require.NoError(t, wrapped.WriteFrame(context.Background(), coderws.MessageText, []byte("downstream")))
	kind, payload, err := wrapped.ReadFrame(context.Background())
	require.NoError(t, err)
	require.Equal(t, coderws.MessageBinary, kind)
	require.Equal(t, []byte("upstream"), payload)
	require.Equal(t, int64(1), inner.writeCalls.Load())
	require.Equal(t, int64(1), inner.readCalls.Load())
	require.GreaterOrEqual(t, reserves.Load(), int64(1))
	require.NoError(t, wrapped.Close())
}
