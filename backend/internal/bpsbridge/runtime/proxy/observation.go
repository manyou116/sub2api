package proxy

import (
	"bytes"
	"context"
	"net/http"
)

// protocolObserver is an opt-in observer for one local diagnostic request. It
// does not retain data, log it, or change what the client receives. Callers must
// treat callback inputs as sensitive and retain only their intended summary.
// Callbacks run synchronously in the existing handshake and stream read paths;
// they must return promptly and protect any state also accessed by their caller.
type protocolObserver struct {
	OnHandshake func(status int, headers http.Header)
	OnEvent     func(event []byte)
}

type protocolObserverContextKey struct{}

func withProtocolObserver(ctx context.Context, observer protocolObserver) context.Context {
	return context.WithValue(ctx, protocolObserverContextKey{}, observer)
}

func protocolObserverFromContext(ctx context.Context) protocolObserver {
	observer, _ := ctx.Value(protocolObserverContextKey{}).(protocolObserver)
	return observer
}

func (o protocolObserver) observeHandshake(resp *http.Response) {
	if o.OnHandshake != nil && resp != nil {
		// A callback must not change rejection handling or the live connection.
		o.OnHandshake(resp.StatusCode, resp.Header.Clone())
	}
}

func (o protocolObserver) observeEvent(data []byte) {
	if o.OnEvent != nil {
		// Preserve original JSON and isolate it from validation and SSE rewriting.
		o.OnEvent(bytes.Clone(data))
	}
}
