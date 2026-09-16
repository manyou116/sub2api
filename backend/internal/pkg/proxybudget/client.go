// Package proxybudget implements the client-side half of the shared proxy
// budget contract. It deliberately has no provider or account knowledge.
package proxybudget

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
)

const (
	EnvURL   = "PROXY_BUDGET_URL"
	EnvToken = "PROXY_BUDGET_TOKEN"

	chunkBytes    int64 = 64 << 10
	overheadBytes int64 = 4 << 10
)

var (
	ErrConfiguration = errors.New("proxy budget configuration is incomplete or invalid")
	ErrDenied        = errors.New("proxy budget denied outbound traffic")
)

type Mode string

const (
	ModeOff     Mode = "off"
	ModeShadow  Mode = "shadow"
	ModeEnforce Mode = "enforce"
)

// DeniedError intentionally stays separate from upstream HTTP errors. Gateway
// callers must not mistake a local admission refusal for an upstream 429.
type DeniedError struct {
	Reason     string
	RetryAfter int
}

func (e *DeniedError) Error() string {
	if e == nil || e.Reason == "" {
		return ErrDenied.Error()
	}
	return fmt.Sprintf("%s: %s", ErrDenied, e.Reason)
}

func (e *DeniedError) Unwrap() error { return ErrDenied }

type Client struct {
	baseURL *url.URL
	token   string
	invalid bool
	http    *http.Client
}

type reserveRequest struct {
	ProxyFingerprint string `json:"proxy_fingerprint"`
	LeaseID          string `json:"lease_id"`
	Sequence         int64  `json:"sequence"`
	Bytes            int64  `json:"bytes"`
}

type reserveResponse struct {
	Allowed      bool   `json:"allowed"`
	Managed      bool   `json:"managed"`
	Mode         Mode   `json:"mode"`
	Reason       string `json:"reason"`
	RetryAfter   int    `json:"retry_after_seconds"`
	GrantedBytes int64  `json:"granted_bytes"`
}

type settleRequest struct {
	ProxyFingerprint string `json:"proxy_fingerprint"`
	LeaseID          string `json:"lease_id"`
	ActualBytes      int64  `json:"actual_bytes"`
}

type settleResponse struct {
	Settled bool `json:"settled"`
}

// NewFromEnv returns a disabled client only when both variables are absent.
// A partial or malformed configuration is retained as invalid so proxied
// traffic fails closed before reaching the upstream.
func NewFromEnv() *Client {
	return New(os.Getenv(EnvURL), os.Getenv(EnvToken), nil)
}

func New(rawURL, token string, httpClient *http.Client) *Client {
	c := &Client{token: strings.TrimSpace(token), http: httpClient}
	if c.http == nil {
		c.http = &http.Client{Timeout: 5 * time.Second}
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" && c.token == "" {
		return c
	}
	if rawURL == "" || c.token == "" {
		c.invalid = true
		return c
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || !validBudgetBaseURL(parsed) {
		c.invalid = true
		return c
	}
	c.baseURL = parsed
	return c
}

func validBudgetBaseURL(u *url.URL) bool {
	if u == nil || u.Hostname() == "" {
		return false
	}
	if strings.EqualFold(u.Scheme, "https") {
		return true
	}
	if !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" {
		return true
	}
	return net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func (c *Client) Disabled() bool { return c == nil || (!c.invalid && c.baseURL == nil) }

func (c *Client) Invalid() bool { return c != nil && c.invalid }

// Fingerprint returns the contract's compact UTF-8 JSON SHA256. It never
// returns the proxy URL or credentials, so callers can safely send its result.
func Fingerprint(rawProxyURL string) (string, error) {
	_, parsed, err := proxyurl.Parse(rawProxyURL)
	if err != nil {
		return "", err
	}
	if parsed == nil {
		return "", nil
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "socks5" {
		scheme = "socks5h"
	}
	port, err := proxyPort(parsed, scheme)
	if err != nil {
		return "", err
	}
	username, password := "", ""
	if parsed.User != nil {
		username = parsed.User.Username()
		password, _ = parsed.User.Password()
	}
	payload, err := json.Marshal([]any{scheme, strings.ToLower(parsed.Hostname()), port, username, password})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func proxyPort(u *url.URL, scheme string) (int, error) {
	if raw := u.Port(); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return 0, errors.New("proxy URL has invalid port")
		}
		return port, nil
	}
	switch scheme {
	case "http":
		return 80, nil
	case "https":
		return 443, nil
	case "socks5h":
		return 1080, nil
	default:
		return 0, errors.New("proxy URL has unsupported scheme")
	}
}

type Lease struct {
	client      *Client
	fingerprint string
	id          string
	mode        Mode
	managed     bool

	mu       sync.Mutex
	sequence int64
	reserved int64
	actual   int64
	settled  bool
}

func (l *Lease) Active() bool  { return l != nil && l.managed && l.mode != ModeOff }
func (l *Lease) Managed() bool { return l != nil && l.managed }
func (l *Lease) Mode() Mode {
	if l == nil {
		return ModeOff
	}
	return l.mode
}
func (l *Lease) ID() string {
	if l == nil {
		return ""
	}
	return l.id
}

// OpenLease admits one outbound attempt. A caller must create a fresh lease
// for each real upstream retry; API retries reuse the request sequence here.
func (c *Client) OpenLease(ctx context.Context, rawProxyURL string) (*Lease, error) {
	if strings.TrimSpace(rawProxyURL) == "" || c.Disabled() {
		return &Lease{}, nil
	}
	if c.Invalid() {
		return nil, ErrConfiguration
	}
	fingerprint, err := Fingerprint(rawProxyURL)
	if err != nil {
		return nil, err
	}
	leaseID, err := randomUUID()
	if err != nil {
		return nil, err
	}
	l := &Lease{client: c, fingerprint: fingerprint, id: leaseID}
	if err := l.reserveLocked(ctx, chunkBytes+overheadBytes); err != nil {
		return nil, err
	}
	return l, nil
}

func randomUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// BeforeRead reserves enough application-byte allowance before exposing more
// proxied body bytes. Returned limits are capped at 64 KiB.
func (l *Lease) BeforeRead(ctx context.Context, requested int) (int, error) {
	if l == nil || !l.Active() || requested <= 0 {
		return requested, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.settled {
		return 0, errors.New("proxy budget lease already settled")
	}
	available := l.reserved - l.actual - overheadBytes
	if available <= 0 {
		if err := l.reserveLocked(ctx, chunkBytes); err != nil {
			return 0, err
		}
		available = l.reserved - l.actual - overheadBytes
	}
	if available <= 0 {
		return 0, &DeniedError{Reason: "budget granted no usable bytes"}
	}
	limit := int64(requested)
	if limit > chunkBytes {
		limit = chunkBytes
	}
	if limit > available {
		limit = available
	}
	return int(limit), nil
}

// ReserveFor admits a bounded payload before the transport can send or expose
// it. Callers without a visible frame size must provide their protocol limit.
func (l *Lease) ReserveFor(ctx context.Context, bytes int64) error {
	if l == nil || !l.Active() || bytes <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.settled {
		return errors.New("proxy budget lease already settled")
	}
	needed := l.actual + bytes + overheadBytes
	for l.reserved < needed {
		reservation := chunkBytes
		if missing := needed - l.reserved; missing > reservation {
			reservation = missing
		}
		if err := l.reserveLocked(ctx, reservation); err != nil {
			return err
		}
	}
	return nil
}

func (l *Lease) Observe(n int) {
	if l == nil || !l.Active() || n <= 0 {
		return
	}
	l.mu.Lock()
	l.actual += int64(n)
	l.mu.Unlock()
}

func (l *Lease) reserveLocked(ctx context.Context, bytes int64) error {
	if bytes <= 0 {
		return errors.New("proxy budget reservation bytes must be positive")
	}
	response, err := l.client.reserve(ctx, reserveRequest{ProxyFingerprint: l.fingerprint, LeaseID: l.id, Sequence: l.sequence, Bytes: bytes})
	if err != nil {
		return err
	}
	if l.sequence == 0 {
		l.mode, l.managed = response.Mode, response.Managed
	}
	if !response.Managed || response.Mode == ModeOff {
		return nil
	}
	if response.Mode == ModeEnforce && !response.Allowed {
		return &DeniedError{Reason: response.Reason, RetryAfter: response.RetryAfter}
	}
	// Shadow records the denial but remains non-blocking by contract.
	if response.GrantedBytes < bytes && response.Mode == ModeEnforce {
		return &DeniedError{Reason: "budget grant smaller than requested", RetryAfter: response.RetryAfter}
	}
	l.reserved += maxInt64(response.GrantedBytes, bytes)
	l.sequence++
	return nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Settle is idempotent locally and relies on the server's lease idempotency
// when a transport retry follows an unknown result.
func (l *Lease) Settle(ctx context.Context) error {
	if l == nil || !l.Active() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.settled {
		return nil
	}
	actual := l.actual + overheadBytes
	if err := l.client.settle(ctx, settleRequest{ProxyFingerprint: l.fingerprint, LeaseID: l.id, ActualBytes: actual}); err != nil {
		return err
	}
	l.settled = true
	return nil
}

func (c *Client) reserve(ctx context.Context, input reserveRequest) (reserveResponse, error) {
	var output reserveResponse
	if err := c.call(ctx, "/reserve", input, &output); err != nil {
		return output, err
	}
	if output.Mode != ModeOff && output.Mode != ModeShadow && output.Mode != ModeEnforce {
		return output, errors.New("proxy budget returned invalid mode")
	}
	return output, nil
}

func (c *Client) settle(ctx context.Context, input settleRequest) error {
	var output settleResponse
	if err := c.call(ctx, "/settle", input, &output); err != nil {
		return err
	}
	if !output.Settled {
		return errors.New("proxy budget settlement was not acknowledged")
	}
	return nil
}

func (c *Client) call(ctx context.Context, path string, input, output any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: strings.TrimRight(c.baseURL.Path, "/") + path})
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Proxy-Budget-Token", c.token)
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("proxy budget request failed: %w", err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode >= 500 && attempt == 0 {
			lastErr = fmt.Errorf("proxy budget status %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("proxy budget status %d", resp.StatusCode)
		}
		if err := json.Unmarshal(body, output); err != nil {
			return fmt.Errorf("proxy budget response: %w", err)
		}
		return nil
	}
	return lastErr
}
