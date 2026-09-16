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
	ErrLeaseRollover = errors.New("proxy budget lease rollover required")
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

type apiError struct {
	Status int
	Code   string
}

func (e *apiError) Error() string { return fmt.Sprintf("proxy budget status %d", e.Status) }

func (e *apiError) Unwrap() error {
	if e != nil && e.Code == "lease_rollover_required" {
		return ErrLeaseRollover
	}
	return nil
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
	port, err := proxyPort(parsed)
	if err != nil {
		return "", err
	}
	username, password := "", ""
	if parsed.User != nil {
		username = parsed.User.Username()
		password, _ = parsed.User.Password()
	}
	payload := compactFingerprintJSON(scheme, strings.ToLower(parsed.Hostname()), port, username, password)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func proxyPort(u *url.URL) (int, error) {
	if raw := u.Port(); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return 0, errors.New("proxy URL has invalid port")
		}
		return port, nil
	}
	// The frozen cross-language contract uses 0 for an omitted port. It does
	// not substitute protocol defaults, so Python and Go hash the same tuple.
	return 0, nil
}

// compactFingerprintJSON emits the contract's compact UTF-8 JSON without the
// standard library's HTML or U+2028/U+2029 escaping. Those escapes are valid
// JSON but would hash differently from Python's ensure_ascii=False encoding.
func compactFingerprintJSON(scheme, host string, port int, username, password string) []byte {
	payload := make([]byte, 0, len(scheme)+len(host)+len(username)+len(password)+32)
	payload = append(payload, '[')
	payload = appendFingerprintString(payload, scheme)
	payload = append(payload, ',')
	payload = appendFingerprintString(payload, host)
	payload = append(payload, ',')
	payload = strconv.AppendInt(payload, int64(port), 10)
	payload = append(payload, ',')
	payload = appendFingerprintString(payload, username)
	payload = append(payload, ',')
	payload = appendFingerprintString(payload, password)
	payload = append(payload, ']')
	return payload
}

func appendFingerprintString(dst []byte, value string) []byte {
	dst = append(dst, '"')
	for _, runeValue := range value {
		switch runeValue {
		case '"', '\\':
			dst = append(dst, '\\', byte(runeValue))
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if runeValue < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', hexDigit(byte(runeValue>>4)), hexDigit(byte(runeValue)))
			} else {
				dst = append(dst, string(runeValue)...)
			}
		}
	}
	return append(dst, '"')
}

func hexDigit(value byte) byte {
	value &= 0x0f
	if value < 10 {
		return '0' + value
	}
	return 'a' + value - 10
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
	pending  int64
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

// IOReservation occupies allowance before one I/O operation begins. Completing
// it converts known bytes to actual use; abandoning it keeps the full pending
// amount, which prevents an interrupted write/read from becoming free traffic.
type IOReservation struct {
	lease *Lease
	bytes int64
	done  sync.Once
}

// ReserveIO reserves a bounded I/O operation before the transport reads or
// writes. Callers with an unknown frame size must pass a protocol maximum.
func (l *Lease) ReserveIO(ctx context.Context, bytes int64) (*IOReservation, error) {
	if l == nil || !l.Active() || bytes <= 0 {
		return &IOReservation{}, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.settled {
		return nil, errors.New("proxy budget lease already settled")
	}
	needed := l.actual + l.pending + bytes + overheadBytes
	for l.reserved < needed {
		reservation := chunkBytes
		if missing := needed - l.reserved; missing > reservation {
			reservation = missing
		}
		if err := l.reserveLocked(ctx, reservation); err != nil {
			return nil, err
		}
	}
	l.pending += bytes
	return &IOReservation{lease: l, bytes: bytes}, nil
}

// Finish accounts known transferred bytes and releases only the unused part of
// a completed operation. Failure must call Unknown, not Finish(0).
func (r *IOReservation) Finish(actual int64) {
	if r == nil || r.lease == nil {
		return
	}
	r.done.Do(func() {
		r.lease.mu.Lock()
		defer r.lease.mu.Unlock()
		if actual < 0 {
			actual = 0
		}
		if actual > r.bytes {
			actual = r.bytes
		}
		r.lease.pending -= r.bytes
		r.lease.actual += actual
	})
}

// Unknown conservatively converts the full operation reservation to consumed
// bytes when the transport can have moved an indeterminate prefix.
func (r *IOReservation) Unknown() {
	if r == nil || r.lease == nil {
		return
	}
	r.done.Do(func() {
		r.lease.mu.Lock()
		defer r.lease.mu.Unlock()
		r.lease.pending -= r.bytes
		r.lease.actual += r.bytes
	})
}

// Observe is retained for shadow-only callers that have no pre-I/O operation.
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
	if errors.Is(err, ErrLeaseRollover) {
		if l.pending != 0 {
			// An active I/O reservation belongs to the old period. Do not swap its
			// mutable lease identity underneath a concurrent completion callback.
			return err
		}
		if settleErr := l.client.settle(ctx, settleRequest{ProxyFingerprint: l.fingerprint, LeaseID: l.id, ActualBytes: l.actual + overheadBytes}); settleErr != nil {
			return settleErr
		}
		newID, idErr := randomUUID()
		if idErr != nil {
			return idErr
		}
		l.id, l.sequence, l.reserved, l.actual = newID, 0, 0, 0
		response, err = l.client.reserve(ctx, reserveRequest{ProxyFingerprint: l.fingerprint, LeaseID: l.id, Sequence: 0, Bytes: bytes})
	}
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
	actual := l.actual + l.pending + overheadBytes
	if err := l.client.settle(ctx, settleRequest{ProxyFingerprint: l.fingerprint, LeaseID: l.id, ActualBytes: actual}); err != nil {
		return err
	}
	l.settled = true
	return nil
}

// WrapBody applies 64 KiB pre-admission to one application body. Only the
// response-side wrapper settles because one HTTP attempt shares one lease.
func WrapBody(body io.ReadCloser, lease *Lease, settleOnFinish bool) io.ReadCloser {
	if body == nil || lease == nil || !lease.Active() {
		return body
	}
	return &budgetingBody{ReadCloser: body, lease: lease, settleOnFinish: settleOnFinish}
}

type budgetingBody struct {
	io.ReadCloser
	lease          *Lease
	settleOnFinish bool
	once           sync.Once
}

func (b *budgetingBody) Read(payload []byte) (int, error) {
	if b == nil || b.ReadCloser == nil {
		return 0, io.ErrClosedPipe
	}
	requested := len(payload)
	if requested > int(chunkBytes) {
		requested = int(chunkBytes)
	}
	reservation, err := b.lease.ReserveIO(context.Background(), int64(requested))
	if err != nil {
		b.settle()
		return 0, err
	}
	read, readErr := b.ReadCloser.Read(payload[:requested])
	if readErr == io.EOF {
		reservation.Finish(int64(read))
		b.settle()
	} else if readErr != nil {
		// Non-EOF source failures can interrupt an in-flight request body. Keep
		// the entire operation charged because the transport may have sent a prefix.
		reservation.Unknown()
		b.settle()
	} else {
		reservation.Finish(int64(read))
	}
	return read, readErr
}

func (b *budgetingBody) Close() error {
	if b == nil || b.ReadCloser == nil {
		return nil
	}
	err := b.ReadCloser.Close()
	b.settle()
	return err
}

func (b *budgetingBody) settle() {
	if b == nil || !b.settleOnFinish {
		return
	}
	b.once.Do(func() { _ = b.lease.Settle(context.Background()) })
}

// WrapRoundTripper reuses one lease for the request and response of one shared
// HTTP attempt. A transport retry enters RoundTrip again and gets a new lease.
func (c *Client) WrapRoundTripper(base http.RoundTripper, rawProxyURL string) http.RoundTripper {
	if c == nil || c.Disabled() || strings.TrimSpace(rawProxyURL) == "" {
		return base
	}
	return budgetRoundTripper{base: base, client: c, proxyURL: rawProxyURL}
}

type budgetRoundTripper struct {
	base     http.RoundTripper
	client   *Client
	proxyURL string
}

func (t budgetRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.base == nil {
		return nil, io.ErrClosedPipe
	}
	ctx := context.Background()
	if req != nil && req.Context() != nil {
		ctx = req.Context()
	}
	lease, err := t.client.OpenLease(ctx, t.proxyURL)
	if err != nil {
		return nil, err
	}
	if req != nil && req.Body != nil {
		req.Body = WrapBody(req.Body, lease, false)
	}
	response, err := t.base.RoundTrip(req)
	if err != nil {
		_ = lease.Settle(context.Background())
		return nil, err
	}
	if response != nil && response.Body != nil {
		response.Body = WrapBody(response.Body, lease, true)
	} else {
		_ = lease.Settle(context.Background())
	}
	return response, nil
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
			var envelope struct {
				Detail struct {
					Code string `json:"code"`
				} `json:"detail"`
			}
			_ = json.Unmarshal(body, &envelope)
			return &apiError{Status: resp.StatusCode, Code: envelope.Detail.Code}
		}
		if err := json.Unmarshal(body, output); err != nil {
			return fmt.Errorf("proxy budget response: %w", err)
		}
		return nil
	}
	return lastErr
}
