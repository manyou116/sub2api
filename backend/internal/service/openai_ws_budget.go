package service

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxybudget"
)

// budgetingOpenAIWSClientDialer keeps managed WebSocket messages behind the
// shared ledger. coder/websocket exposes complete messages, so a read reserves
// the configured 16 MiB message cap before handing data to its caller.
type budgetingOpenAIWSClientDialer struct {
	next   openAIWSClientDialer
	budget *proxybudget.Client
}

func newBudgetingOpenAIWSClientDialer(next openAIWSClientDialer) openAIWSClientDialer {
	budget := proxybudget.NewFromEnv()
	if budget.Disabled() {
		return next
	}
	return &budgetingOpenAIWSClientDialer{next: next, budget: budget}
}

func (d *budgetingOpenAIWSClientDialer) Dial(ctx context.Context, wsURL string, headers http.Header, proxyURL string) (openAIWSClientConn, int, http.Header, error) {
	if d == nil || d.next == nil {
		return nil, 0, nil, errOpenAIWSConnClosed
	}
	lease, err := d.budget.OpenLease(ctx, proxyURL)
	if err != nil {
		return nil, 0, nil, err
	}
	conn, status, responseHeaders, err := d.next.Dial(ctx, wsURL, headers, proxyURL)
	if err != nil {
		_ = lease.Settle(context.Background())
		return nil, status, responseHeaders, err
	}
	if !lease.Active() {
		return conn, status, responseHeaders, nil
	}
	return &budgetingOpenAIWSClientConn{next: conn, lease: lease}, status, responseHeaders, nil
}

// budgetingOpenAIWSClientConn admits writes at their encoded size. Reads use
// the local protocol cap because their size is unknown until the frame arrives.
type budgetingOpenAIWSClientConn struct {
	next  openAIWSClientConn
	lease *proxybudget.Lease
	once  sync.Once
}

func (c *budgetingOpenAIWSClientConn) WriteJSON(ctx context.Context, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	reservation, err := c.lease.ReserveIO(ctx, int64(len(payload)))
	if err != nil {
		c.settle()
		_ = c.next.Close()
		return err
	}
	err = c.next.WriteJSON(ctx, value)
	if err == nil {
		reservation.Finish(int64(len(payload)))
	}
	if err != nil {
		// A websocket write can fail after an unknown prefix reached the proxy.
		reservation.Unknown()
		c.settle()
	}
	return err
}

func (c *budgetingOpenAIWSClientConn) ReadMessage(ctx context.Context) ([]byte, error) {
	reservation, err := c.lease.ReserveIO(ctx, openAIWSMessageReadLimitBytes)
	if err != nil {
		c.settle()
		_ = c.next.Close()
		return nil, err
	}
	payload, err := c.next.ReadMessage(ctx)
	if err == nil {
		reservation.Finish(int64(len(payload)))
	} else {
		reservation.Unknown()
	}
	if err != nil {
		c.settle()
	}
	return payload, err
}

func (c *budgetingOpenAIWSClientConn) Ping(ctx context.Context) error {
	// Ping frames bypass JSON accounting, so reserve a conservative control-frame allowance.
	reservation, err := c.lease.ReserveIO(ctx, 256)
	if err != nil {
		c.settle()
		_ = c.next.Close()
		return err
	}
	err = c.next.Ping(ctx)
	if err == nil {
		reservation.Finish(256)
	} else {
		reservation.Unknown()
		c.settle()
	}
	return err
}

func (c *budgetingOpenAIWSClientConn) Close() error {
	err := c.next.Close()
	c.settle()
	return err
}

func (c *budgetingOpenAIWSClientConn) settle() {
	if c == nil {
		return
	}
	c.once.Do(func() { _ = c.lease.Settle(context.Background()) })
}
