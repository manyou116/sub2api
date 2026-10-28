package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxybudget"
	coderws "github.com/coder/websocket"
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

// openAIWSBudgetStreamReadable is implemented by the default coder client.
// Test injectors and alternate clients retain the bounded one-message fallback.
type openAIWSBudgetStreamReadable interface {
	ReadMessageBudgeted(context.Context, func(context.Context, int64) (*proxybudget.IOReservation, error)) ([]byte, error)
}

type openAIWSBudgetFrameReadable interface {
	ReadFrameBudgeted(context.Context, func(context.Context, int64) (*proxybudget.IOReservation, error)) (coderws.MessageType, []byte, error)
}

type openAIWSFrameWritable interface {
	WriteFrame(context.Context, coderws.MessageType, []byte) error
}

func (c *budgetingOpenAIWSClientConn) SupportsIdlePingWithoutReader() bool {
	if c == nil {
		return false
	}
	capable, ok := c.next.(openAIWSIdlePingCapable)
	return ok && capable.SupportsIdlePingWithoutReader()
}

func (c *budgetingOpenAIWSClientConn) RequiresReaderLoop() bool {
	capable, ok := c.next.(openAIWSReaderLoopCapable)
	return ok && capable.RequiresReaderLoop()
}

func (c *budgetingOpenAIWSClientConn) UpstreamPingCount() int64 {
	if counter, ok := c.next.(openAIWSUpstreamPingCounter); ok {
		return counter.UpstreamPingCount()
	}
	return 0
}

func (c *budgetingOpenAIWSClientConn) CloseNow() error {
	if closer, ok := c.next.(openAIWSForceCloser); ok {
		err := closer.CloseNow()
		c.settle()
		return err
	}
	return c.Close()
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
	if streamed, ok := c.next.(openAIWSBudgetStreamReadable); ok {
		payload, err := streamed.ReadMessageBudgeted(ctx, c.lease.ReserveIO)
		if err != nil {
			c.settle()
		}
		return payload, err
	}
	// Non-default injected clients expose complete messages only. Preserve the
	// established 16 MiB local cap as a finite, conservative fallback.
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

func (c *budgetingOpenAIWSClientConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	if streamed, ok := c.next.(openAIWSBudgetFrameReadable); ok {
		kind, payload, err := streamed.ReadFrameBudgeted(ctx, c.lease.ReserveIO)
		if err != nil {
			c.settle()
		}
		return kind, payload, err
	}
	return coderws.MessageText, nil, errOpenAIWSConnClosed
}

func (c *budgetingOpenAIWSClientConn) WriteFrame(ctx context.Context, kind coderws.MessageType, payload []byte) error {
	writer, ok := c.next.(openAIWSFrameWritable)
	if !ok {
		return errOpenAIWSConnClosed
	}
	reservation, err := c.lease.ReserveIO(ctx, int64(len(payload)))
	if err != nil {
		c.settle()
		_ = c.next.Close()
		return err
	}
	err = writer.WriteFrame(ctx, kind, payload)
	if err == nil {
		reservation.Finish(int64(len(payload)))
	} else {
		reservation.Unknown()
		c.settle()
	}
	return err
}

// ReadMessageBudgeted streams the normal coder/websocket reader in fixed
// chunks. The existing 16 MiB SetReadLimit remains the protocol maximum while
// each 64 KiB application chunk is admitted before it is copied to the caller.
func (c *coderOpenAIWSClientConn) ReadMessageBudgeted(ctx context.Context, reserve func(context.Context, int64) (*proxybudget.IOReservation, error)) ([]byte, error) {
	_, payload, err := c.ReadFrameBudgeted(ctx, reserve)
	return payload, err
}

func (c *coderOpenAIWSClientConn) ReadFrameBudgeted(ctx context.Context, reserve func(context.Context, int64) (*proxybudget.IOReservation, error)) (coderws.MessageType, []byte, error) {
	if c == nil || c.conn == nil {
		return coderws.MessageText, nil, errOpenAIWSConnClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	messageType, reader, err := c.conn.Reader(ctx)
	if err != nil {
		return coderws.MessageText, nil, err
	}
	if messageType != coderws.MessageText && messageType != coderws.MessageBinary {
		return coderws.MessageText, nil, errOpenAIWSConnClosed
	}
	payload := make([]byte, 0, proxybudget.ChunkBytes)
	buffer := make([]byte, proxybudget.ChunkBytes)
	for {
		reservation, reserveErr := reserve(ctx, int64(len(buffer)))
		if reserveErr != nil {
			return messageType, nil, reserveErr
		}
		read, readErr := reader.Read(buffer)
		if read > 0 {
			payload = append(payload, buffer[:read]...)
		}
		if readErr == io.EOF {
			reservation.Finish(int64(read))
			return messageType, payload, nil
		}
		if readErr != nil {
			reservation.Unknown()
			return messageType, nil, readErr
		}
		reservation.Finish(int64(read))
	}
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
