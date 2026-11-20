// Package proxy implements the local Responses-API front end that bridges Codex
// CLI to the ChatGPT Excel (Basispoints) gateway.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/auth"
	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/basispoints"
	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/sanitize"
)

// Config describes how the front end reaches the upstream gateway.
type Config struct {
	ImageRelay *basispoints.ImageRelay
	Headers    map[string]string
	// ARCTextSession enables experimental ARC initialization for text-only requests.
	ARCTextSession bool
	// Upstream is the Basispoints Responses endpoint.
	Upstream string
	// UpstreamTransport is "ws" for official BPS. Legacy "sse" is retained
	// only for explicitly configured alternative upstreams and offline fixtures.
	UpstreamTransport string
	// WSIdleTimeout bounds a wait for the next upstream data/control message.
	WSIdleTimeout time.Duration
	// ToolsVersion optionally pins metadata.bps_tools_version_id. Empty lets
	// the server choose; captured version IDs are not hardcoded.
	ToolsVersion string
	// ProxyURL routes outbound traffic, e.g. http://127.0.0.1:7890. Empty
	// means direct.
	ProxyURL string
	// Verbose logs each request line, never prompt content or tokens.
	Verbose bool
	// Zero RPS disables pacing. Queue/wait zero values select 64 and 30 seconds.
	UpstreamRPS     int
	RateQueueLimit  int
	RateWaitTimeout time.Duration
}

// CredentialSource supplies an isolated credential snapshot.
type CredentialSource interface {
	Credentials() (string, string, error)
	Expiry() time.Time
}

// Server is the local bridge.
type Server struct {
	cfg                Config
	store              CredentialSource
	client             *http.Client
	replay             *basispoints.ReplayCache
	startRates         *startRates
	dialRates          *startRates
	startAuthorization startAuthorization
	startDeadline      time.Time
}

// New builds a server. It fails early when credentials are unusable so the user
// does not discover the problem mid-request.
func New(cfg Config, store CredentialSource) (*Server, error) {
	rates, err := newStartRates(cfg)
	if err != nil {
		return nil, err
	}
	dialRates, err := newStartRates(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Upstream == "" {
		cfg.Upstream = basispoints.ResponsesURL
	}
	if cfg.UpstreamTransport == "" {
		cfg.UpstreamTransport = "ws"
	}
	if cfg.UpstreamTransport != "ws" && cfg.UpstreamTransport != "sse" {
		return nil, fmt.Errorf("upstream transport must be ws or sse")
	}
	u, err := url.Parse(cfg.Upstream)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("invalid upstream URL")
	}
	if cfg.UpstreamTransport == "sse" && strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "bps.openai.com") {
		return nil, fmt.Errorf("official BPS requires upstream transport ws; legacy POST/SSE is unsupported (the local Responses API still uses HTTP/SSE)")
	}
	switch u.Scheme {
	case "https", "wss":
	case "http", "ws":
		if !IsLoopback(u.Host) {
			return nil, fmt.Errorf("unencrypted upstream is allowed only on loopback")
		}
	default:
		return nil, fmt.Errorf("upstream must use https, wss, http or ws")
	}
	if cfg.WSIdleTimeout == 0 {
		cfg.WSIdleTimeout = 2 * time.Minute
	}
	if cfg.WSIdleTimeout < 0 {
		return nil, fmt.Errorf("WebSocket idle timeout must be positive")
	}
	transport := &http.Transport{
		// RFC 6455 upgrades use HTTP/1.1, including through CONNECT proxies.
		ForceAttemptHTTP2:   cfg.UpstreamTransport == "sse",
		MaxIdleConns:        32,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		// Basispoints streams slowly; do not cap the whole request.
		ResponseHeaderTimeout: 60 * time.Second,
	}
	if cfg.ProxyURL != "" {
		parsed, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid --proxy %q: %w", cfg.ProxyURL, err)
		}
		transport.Proxy = http.ProxyURL(parsed)
		if cfg.Verbose {
			log.Printf("outbound proxy: %s", parsed.Redacted())
		}
	}

	return &Server{
		cfg:        cfg,
		store:      store,
		replay:     &basispoints.ReplayCache{},
		startRates: rates,
		dialRates:  dialRates,
		client: &http.Client{
			Transport: transport,
			// Never follow a redirect with a bearer subprotocol or account headers.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			// No global timeout: streaming responses may legitimately run long.
			Timeout: 0,
		},
	}, nil
}

// Handler returns the mux serving the Responses API surface Codex expects.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", s.handleResponses)
	mux.HandleFunc("/responses", s.handleResponses)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _, err := s.store.Credentials()
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"ok":false,"error":`+jsonString(err.Error())+`}`)
			return
		}
		expiry := s.store.Expiry()
		body := fmt.Sprintf(`{"ok":true,"token_expires_at":%q}`, expiry.Format(time.RFC3339))
		_, _ = io.WriteString(w, body)
	})
	return mux
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":{"message":"method not allowed"}}`, http.StatusMethodNotAllowed)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 256<<20))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
		return
	}

	if s.cfg.Verbose {
		log.Printf("incoming: %s", summarizeRequest(raw))
	}
	if s.cfg.ARCTextSession && !arcTextOnly(raw) {
		s.fail(w, http.StatusBadRequest, "arc_text_only", "ARC text sessions support messages only, with no client tools")
		return
	}

	// This bridge supports client tools only; upstream-native hosted tools have
	// different contracts. Keep the existing sanitization for both transports.
	raw, removed, err := sanitize.Apply(raw)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "basispoints_request_invalid", err.Error())
		return
	}
	if !removed.Empty() && s.cfg.Verbose {
		log.Printf("stripped server tools: %v", removed.RemovedServerTools)
	}

	options := basispoints.PrepareOptions{SessionID: logicalSessionFor(r)}
	upstreamIdentity, hasUpstreamIdentity := upstreamTaskFor(r)
	if hasUpstreamIdentity {
		if upstreamIdentity.taskID == "" && upstreamIdentity.turnID == "" {
			s.fail(w, http.StatusBadRequest, "basispoints_request_invalid", "invalid server-owned upstream task identity")
			return
		}
		options.UpstreamTaskID, options.UpstreamTurnID = upstreamIdentity.taskID, upstreamIdentity.turnID
	}
	if err := options.Validate(); err != nil {
		s.fail(w, http.StatusBadRequest, "basispoints_request_invalid", err.Error())
		return
	}
	scope := s.scopeFor(r)
	if s.cfg.ImageRelay != nil {
		raw, err = s.cfg.ImageRelay.Rewrite(raw, scope)
		if err != nil {
			status, code := http.StatusBadRequest, "basispoints_request_invalid"
			if errors.Is(err, basispoints.ErrImageRelayFull) || errors.Is(err, basispoints.ErrImageRelayStorage) {
				status, code = http.StatusServiceUnavailable, "basispoints_image_relay_unavailable"
			}
			s.fail(w, status, code, err.Error())
			return
		}
	}
	upstreamBody, bridge, err := basispoints.PrepareWithOptions(raw, scope, s.replay, options)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "basispoints_request_invalid", err.Error())
		return
	}
	if hasUpstreamIdentity {
		writeUpstreamTaskHashes(w.Header(), upstreamBody)
	}

	token, accountID, err := s.store.Credentials()
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "basispoints_auth_unavailable", err.Error())
		return
	}
	if accountID == "" {
		accountID = auth.AccountIDFromToken(token)
	}
	if accountID == "" {
		s.fail(w, http.StatusUnauthorized, "basispoints_account_id_missing",
			"the BPS account id is missing; check the independent OAuth file")
		return
	}

	rateWait := &startRateWaitObservation{}
	ctx := context.WithValue(r.Context(), startRateWaitKey{}, rateWait)
	wsTiming := &webSocketTiming{}
	ctx = context.WithValue(ctx, webSocketTimingKey{}, wsTiming)
	w.Header().Set("X-BPS-Prepare-Ms", strconv.FormatInt(time.Since(start).Milliseconds(), 10))
	openStarted := time.Now()
	upstream, err := s.openUpstream(ctx, upstreamBody, token, accountID)
	wsTiming.writeHeaders(w.Header())
	w.Header().Set("X-BPS-Rate-Wait-Ms", strconv.FormatInt(rateWait.elapsed.Milliseconds(), 10))
	w.Header().Set("X-BPS-Upstream-Open-Ms", strconv.FormatInt(time.Since(openStarted).Milliseconds(), 10))
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		status := http.StatusBadGateway
		code := "basispoints_transport_error"
		var pacing *StartRateError
		if errors.As(err, &pacing) {
			status, code = pacing.Status, pacing.Code
			if status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "1")
			}
		}
		var rejected *upstreamRejection
		if errors.As(err, &rejected) {
			code = "basispoints_upstream_error"
			if rejected.status >= 400 && rejected.status <= 599 {
				status = rejected.status
			}
			if rejected.retry > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(rejected.retry))
			}
		}
		s.fail(w, status, code, err.Error())
		return
	}
	defer func() { _ = upstream.Close() }()

	// The local Responses interface stays SSE; only the upstream transport changes.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	translated := bridge.Stream(upstream)
	defer func() { _ = translated.Close() }()

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, readErr := translated.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF && s.cfg.Verbose {
				log.Printf("stream ended: %v (after %s)", readErr, time.Since(start).Round(time.Millisecond))
			}
			return
		}
	}
}

// scope isolates replayed tool identities per credential and session so two
// concurrent conversations cannot observe each other's call ids.
func (s *Server) scopeFor(r *http.Request) string {
	if sid := strings.TrimSpace(r.Header.Get("session_id")); sid != "" {
		return sid
	}
	return "local"
}

func applyUpstreamHeaders(req *http.Request, token, accountID string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Chatgpt-Account-Id", accountID)
	req.Header.Set("X-Openai-Account-Id", accountID)
	req.Header.Set("X-Basispoints-Auth-Mode", "chatgpt")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", "https://bps.openai.com")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("X-Openai-Internal-Basispoints-Client-Product", "basispoints-excel-plugin")
	req.Header.Set("X-Openai-Internal-Basispoints-Client-Agent-Profile", "excel")
}

func (s *Server) fail(w http.ResponseWriter, status int, code, message string) {
	if s.cfg.Verbose {
		log.Printf("rejecting request: %s", code)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":`+jsonString(code)+`,"message":`+jsonString(message)+`}}`)
}

// summarizeRequest reports the field names Codex sent, plus the signals that
// decide whether the Basispoints translation can succeed. Prompt text, tool
// descriptions and credentials are never included.
func summarizeRequest(raw []byte) string {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Sprintf("unparseable body (%d bytes)", len(raw))
	}
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var notes []string
	if v, ok := doc["previous_response_id"].(string); ok && v != "" {
		notes = append(notes, "previous_response_id=PRESENT")
	}
	if tools, ok := doc["tools"].([]any); ok {
		kinds := make([]string, 0, len(tools))
		for _, t := range tools {
			m, _ := t.(map[string]any)
			kind, _ := m["type"].(string)
			name, _ := m["name"].(string)
			if name != "" {
				kinds = append(kinds, kind+":"+name)
			} else {
				kinds = append(kinds, kind)
			}
		}
		notes = append(notes, fmt.Sprintf("tools=%d%v", len(tools), kinds))
	}
	if input, ok := doc["input"].([]any); ok {
		notes = append(notes, fmt.Sprintf("input=%d items", len(input)))
	}
	if v, ok := doc["tool_choice"]; ok {
		notes = append(notes, fmt.Sprintf("tool_choice=%v", v))
	}
	return fmt.Sprintf("fields=%v %s", keys, strings.Join(notes, " "))
}

// IsLoopback reports whether addr refers to the loopback interface. It accepts
// a bare host, host:port, or bracketed IPv6 form.
func IsLoopback(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return true
	}

	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	} else if ip := net.ParseIP(addr); ip != nil {
		// A bare address with no port, e.g. "127.0.0.1" or "::1".
		host = addr
	} else if i := strings.LastIndex(addr, ":"); i >= 0 && !strings.Contains(addr, "::") {
		// host:port where the port is not numeric, e.g. "localhost:http".
		host = addr[:i]
	}

	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func jsonString(v string) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(raw)
}

// WithIsolatedReplay gives a request its own tool-identity cache. Standalone
// callers have no authenticated conversation namespace, so they must supply
// full tool history instead of borrowing a prior caller's cached tool call.
func (s *Server) WithIsolatedReplay() *Server {
	copy := *s
	copy.replay = &basispoints.ReplayCache{}
	return &copy
}

// WithCredentials shares transport and replay cache, never mutable credentials.
func (s *Server) WithCredentials(source CredentialSource) http.Handler {
	copy := *s
	copy.store = source
	return copy.Handler()
}

// WithProxyURL isolates a request's transport while retaining the shared replay cache.
func (s *Server) WithProxyURL(address string) (*Server, func(), error) {
	cfg := s.cfg
	cfg.ProxyURL = address
	next, err := New(cfg, s.store)
	if err != nil {
		return nil, nil, err
	}
	next.replay = s.replay
	next.startRates, next.dialRates = s.startRates, s.dialRates
	next.startAuthorization, next.startDeadline = s.startAuthorization, s.startDeadline
	return next, next.client.CloseIdleConnections, nil
}

// WithHeaders returns an immutable request-scoped header configuration.
func (s *Server) WithHeaders(headers map[string]string) *Server {
	copy := *s
	copy.cfg.Headers = headers
	return &copy
}
func (s *Server) applyCustomHeaders(req *http.Request) {
	for key, value := range s.cfg.Headers {
		k := strings.ToLower(key)
		if k == "authorization" || k == "host" || k == "cookie" || k == "origin" || k == "connection" || k == "upgrade" || k == "content-length" || k == "content-type" || k == "accept" || strings.HasPrefix(k, "sec-") || strings.HasPrefix(k, "proxy-") || strings.HasPrefix(k, "x-openai-") || strings.HasPrefix(k, "x-basispoints-") || k == "chatgpt-account-id" {
			continue
		}
		req.Header.Set(key, value)
	}
}

// SetImageRelay must be called before serving requests.
func (s *Server) SetImageRelay(relay *basispoints.ImageRelay) { s.cfg.ImageRelay = relay }
