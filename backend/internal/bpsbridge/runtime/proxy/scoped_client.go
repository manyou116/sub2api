package proxy

import (
	"context"
	"net"
	"net/http"
)

// net/http may detach a connection dial from the request context so another
// request can reuse it. A WebSocket upgrade is exclusive: tie its raw socket to
// this request as well, including CONNECT and TLS before the upgrade completes.
func scopedWebSocketClient(ctx context.Context, client *http.Client) (*http.Client, func()) {
	base, ok := client.Transport.(*http.Transport)
	if !ok {
		return client, func() {}
	}
	scope, cancel := context.WithCancel(ctx)
	transport := base.Clone()
	dial := base.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	transport.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		combined, stop := context.WithCancel(dialCtx)
		stopScope := context.AfterFunc(scope, stop)
		defer stop()
		defer stopScope()
		if scope.Err() != nil {
			return nil, scope.Err()
		}
		conn, err := dial(combined, network, address)
		if err != nil {
			return nil, err
		}
		bound := &scopedConn{Conn: conn}
		bound.stop = context.AfterFunc(scope, func() { _ = conn.Close() })
		if scope.Err() != nil {
			_ = bound.Close()
			return nil, scope.Err()
		}
		return bound, nil
	}
	copyClient := *client
	copyClient.Transport = transport
	return &copyClient, func() { cancel(); transport.CloseIdleConnections() }
}

type scopedConn struct {
	net.Conn
	stop func() bool
}

func (c *scopedConn) Close() error {
	if c.stop != nil {
		c.stop()
	}
	return c.Conn.Close()
}
