package bpsbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/auth"
	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/gateway"
	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/proxy"
)

var ErrUnavailable = errors.New("BPS runtime is unavailable")

type Runtime struct {
	Dir   string
	Proxy string
}

type Prompt struct {
	URL  string `json:"url"`
	Code string `json:"code"`
}

const deviceURL = issuer + "/api/accounts/deviceauth/authorize?client_id=" + ClientID

func (r Runtime) Device(ctx context.Context, show func(Prompt)) ([]byte, error) {
	client, err := auth.NewDeviceClient(r.Proxy)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer client.HTTP.CloseIdleConnections()
	return deviceGrant(ctx, client, show)
}

func deviceGrant(ctx context.Context, client *auth.DeviceClient, show func(Prompt)) ([]byte, error) {
	credentials, err := client.Login(ctx, func(prompt auth.DevicePrompt) error {
		if prompt.URL != deviceURL || len(prompt.UserCode) < 4 || len(prompt.UserCode) > 32 {
			return ErrUnavailable
		}
		for _, c := range prompt.UserCode {
			if c != '-' && (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
				return ErrUnavailable
			}
		}
		show(Prompt{URL: prompt.URL, Code: prompt.UserCode})
		return nil
	})
	if err != nil {
		return nil, ErrUnavailable
	}
	return json.Marshal(credentials)
}

// fileCredentials reads only Sub2API's private grant for this shadow.
// Atomic replacement is visible to the next request on a cached worker.
type fileCredentials string

func (f fileCredentials) grant() BoundGrant {
	var bound BoundGrant
	if data, err := ReadGrantFile(string(f)); err == nil && json.Unmarshal(data, &bound) == nil {
		return bound
	}
	return BoundGrant{}
}

func (f fileCredentials) Credentials() (string, string, error) {
	bound := f.grant()
	if bound.AccessToken == "" || bound.Identity.Account == "" || !time.Now().Before(bound.ExpiresAt) {
		return "", "", ErrInvalidGrant
	}
	return bound.AccessToken, bound.Identity.Account, nil
}

func (f fileCredentials) Expiry() time.Time { return f.grant().ExpiresAt }

type Worker struct {
	handler http.Handler
	ctx     context.Context
	cancel  context.CancelFunc
	close   func()
	once    sync.Once
}

func (r Runtime) Start(ctx context.Context, authPath, stateDir string, concurrency int) (*Worker, error) {
	return r.start(ctx, authPath, stateDir, concurrency, proxy.Config{
		ProxyURL: r.Proxy, UpstreamTransport: "ws",
	})
}

func (r Runtime) start(ctx context.Context, authPath, stateDir string, concurrency int, cfg proxy.Config) (*Worker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store := fileCredentials(authPath)
	if _, _, err := store.Credentials(); err != nil {
		return nil, err
	}
	state, err := gateway.OpenStandaloneState(stateDir)
	if err != nil {
		return nil, ErrUnavailable
	}
	server, err := proxy.New(cfg, store)
	if err != nil {
		state.Close()
		return nil, ErrUnavailable
	}
	handler, err := gateway.NewStandaloneHandler(server, store, state.Key(), gateway.Limits{
		PerAccount: concurrency, Wait: 30 * time.Second,
	})
	if err != nil {
		server.CloseStartRate()
		state.Close()
		return nil, ErrUnavailable
	}
	return newWorker(handler, func() {
		handler.Close()
		handler.Wait()
		state.Close()
	}), nil
}

func newWorker(handler http.Handler, closeRuntime func()) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Worker{handler: handler, ctx: ctx, cancel: cancel, close: closeRuntime}
}

func (w *Worker) Done() <-chan struct{} { return w.ctx.Done() }

func (w *Worker) Stop() {
	w.once.Do(func() {
		w.cancel()
		w.close()
	})
}

// Responses preserves the streaming boundary while calling the embedded
// handler directly. The pipe applies backpressure and buffers no response body.
func (w *Worker) Responses(ctx context.Context, grant BoundGrant, body []byte) (*http.Response, error) {
	if grant.AccessToken == "" || grant.Identity.Account == "" || !time.Now().Before(grant.ExpiresAt) {
		return nil, ErrInvalidGrant
	}
	ctx = gateway.WithRequestCredentials(ctx, grant.AccessToken, grant.Identity.Account)
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(w.ctx, cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if err != nil {
		stop()
		cancel()
		return nil, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	reader, writer := io.Pipe()
	cancelPipe := context.AfterFunc(ctx, func() { _ = writer.CloseWithError(ctx.Err()) })
	exchange := &responsePipe{header: make(http.Header), ready: make(chan *http.Response, 1), writer: writer,
		body: &responseBody{PipeReader: reader, cancel: cancel}}
	go func() {
		defer cancel()
		defer stop()
		defer cancelPipe()
		defer func() {
			if recover() != nil {
				_ = writer.CloseWithError(ErrUnavailable)
				exchange.WriteHeader(http.StatusBadGateway)
			}
		}()
		w.handler.ServeHTTP(exchange, req)
		exchange.WriteHeader(http.StatusOK)
		_ = writer.Close()
	}()
	select {
	case resp := <-exchange.ready:
		return resp, nil
	case <-ctx.Done():
		select {
		case resp := <-exchange.ready:
			return resp, nil
		default:
		}
		_ = exchange.body.Close()
		return nil, ctx.Err()
	}
}

type responsePipe struct {
	header http.Header
	ready  chan *http.Response
	writer *io.PipeWriter
	body   io.ReadCloser
	sent   bool
}

func (p *responsePipe) Header() http.Header { return p.header }

func (p *responsePipe) WriteHeader(code int) {
	if !p.sent {
		p.sent = true
		p.ready <- &http.Response{StatusCode: code, Header: p.header.Clone(), Body: p.body}
	}
}

func (p *responsePipe) Write(data []byte) (int, error) {
	p.WriteHeader(http.StatusOK)
	return p.writer.Write(data)
}

func (p *responsePipe) Flush() { p.WriteHeader(http.StatusOK) }

type responseBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r *responseBody) Close() error {
	r.cancel()
	return r.PipeReader.Close()
}
