package service

import (
	"context"
	"encoding/json"
	"net/http"

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
	ended bool
}

func (c *budgetingOpenAIWSClientConn) WriteJSON(ctx context.Context, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := c.lease.ReserveFor(ctx, int64(len(payload))); err != nil {
		c.settle()
		_ = c.next.Close()
		return err
	}
	err = c.next.WriteJSON(ctx, value)
	if err == nil {
		c.lease.Observe(len(payload))
	}
	if err != nil {
		c.settle()
	}
	return err
}

func (c *budgetingOpenAIWSClientConn) ReadMessage(ctx context.Context) ([]byte, error) {
	if err := c.lease.ReserveFor(ctx, openAIWSMessageReadLimitBytes); err != nil {
		c.settle()
		_ = c.next.Close()
		return nil, err
	}
	payload, err := c.next.ReadMessage(ctx)
	c.lease.Observe(len(payload))
	if err != nil {
		c.settle()
	}
	return payload, err
}

func (c *budgetingOpenAIWSClientConn) Ping(ctx context.Context) error { return c.next.Ping(ctx) }

func (c *budgetingOpenAIWSClientConn) Close() error {
	err := c.next.Close()
	c.settle()
	return err
}

func (c *budgetingOpenAIWSClientConn) settle() {
	if c == nil || c.ended {
		return
	}
	c.ended = true
	_ = c.lease.Settle(context.Background())
}
