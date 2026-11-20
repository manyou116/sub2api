package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/retryafter"
	"github.com/coder/websocket"
)

const maxHandshakeAttempts = 3

// dialWebSocket retries only classified transient failures before a successful
// HTTP upgrade. No response.create has been written at this seam, so reconnecting
// cannot replay a generation or tool operation. All attempts share one deadline.
func (s *Server) dialWebSocket(ctx context.Context, address string, header http.Header, token, admissionKey string) (*websocket.Conn, func(), func(), error) {
	dialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	timing := webSocketTimingFromContext(ctx)
	dialWait := &startRateWaitObservation{}
	dialWaitCtx := context.WithValue(dialCtx, startRateWaitKey{}, dialWait)
	timing.handshakeWaitObserved = true
	defer func() { timing.handshakeWait = dialWait.elapsed }()
	for attempt := 1; ; attempt++ {
		// Every attempt owns its connections; never recycle a failed connection
		// or close another concurrent request's transport. The successful socket
		// follows the request lifetime, not the shorter handshake budget.
		client, release := scopedWebSocketClient(ctx, s.client)
		upgradeTransport := &webSocketUpgradeTransport{base: client.Transport}
		if upgradeTransport.base == nil {
			upgradeTransport.base = http.DefaultTransport
		}
		requestClient := *client
		requestClient.Transport = upgradeTransport
		var conn *websocket.Conn
		var resp *http.Response
		err := s.dialRates.runDispatch(dialWaitCtx, admissionKey, s.startAuthorization, s.startDeadline, func(releaseDispatch func()) error {
			// Each retry consumes a dial start and rechecks authorization. Release
			// before network I/O so slow handshakes can overlap permitted dials.
			if timing.dialStart.IsZero() {
				timing.dialStart = time.Now()
			}
			releaseDispatch()
			var dialErr error
			conn, resp, dialErr = websocket.Dial(dialCtx, address, &websocket.DialOptions{
				HTTPClient: &requestClient, HTTPHeader: header,
				Subprotocols: []string{"responses", "openai-bearer." + token},
			})
			timing.dialDone = time.Now()
			if resp != nil {
				timing.handshakeStatus = resp.StatusCode
			}
			return dialErr
		})
		protocolObserverFromContext(ctx).observeHandshake(resp)
		if err == nil {
			return conn, release, upgradeTransport.abort, nil
		}
		if conn != nil {
			_ = conn.CloseNow()
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		upgradeTransport.abort()
		release()
		var pacing *StartRateError
		if errors.As(err, &pacing) {
			return nil, nil, nil, err
		}
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		// Preserve explicit upstream status/Retry-After. Authentication errors,
		// rate limits, redirects and HTTP rejections are never hidden by retries.
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, nil, nil, &upstreamRejection{status: resp.StatusCode, retry: retryafter.Seconds(resp.Header.Get("Retry-After"))}
		}
		reason, transient := handshakeFailureKind(err)
		if resp != nil {
			reason, transient = "invalid_upgrade", false
		}
		retry := transient && attempt < maxHandshakeAttempts && dialCtx.Err() == nil && ctx.Value(protocolObserverContextKey{}) == nil
		if s.cfg.Verbose {
			log.Printf("upstream ws handshake_failed reason=%s attempt=%d retry=%t request_not_sent=true", reason, attempt, retry)
		}
		// Never format the underlying error: it may contain a bearer protocol,
		// redirect URL, proxy credentials or upstream-controlled response text.
		failure := fmt.Errorf("WebSocket handshake failed (reason=%s; attempts=%d; request_not_sent=true); check proxy, credentials and upstream WebSocket support", reason, attempt)
		if !retry {
			return nil, nil, nil, failure
		}
		timer := time.NewTimer(time.Duration(attempt) * 250 * time.Millisecond)
		select {
		case <-dialCtx.Done():
			timer.Stop()
			return nil, nil, nil, failure
		case <-timer.C:
		}
	}
}

// Return only fixed categories. Unknown/policy failures do not become retryable
// merely because their human-readable messages contain network-looking words.
func handshakeFailureKind(err error) (string, bool) {
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return "tls_certificate", false
	}
	if errors.Is(err, context.Canceled) {
		return "canceled", false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "network_eof", true
	}
	// Windows socket APIs return Winsock values rather than the portable
	// syscall.ECONN* placeholders. Values are defined by Winsock/Go syscall.
	if runtime.GOOS == "windows" {
		if errors.Is(err, syscall.Errno(10054)) || errors.Is(err, syscall.Errno(10053)) {
			return "connection_interrupted", true
		}
		if errors.Is(err, syscall.Errno(10061)) {
			return "connection_refused", false
		}
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return "connection_interrupted", true
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused", false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns", dns.IsTimeout || dns.IsTemporary
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "network_timeout", true
	}
	return "unclassified", false
}
