package openaiimages

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// dispAccount is a minimal AccountView for dispatch tests.
type dispAccount struct {
	id     int64
	apikey bool
	web    bool
}

func (a *dispAccount) ID() int64                            { return a.id }
func (a *dispAccount) AccessToken() string                  { return "tok" }
func (a *dispAccount) ChatGPTAccountID() string             { return "" }
func (a *dispAccount) UserAgent() string                    { return "" }
func (a *dispAccount) DeviceID() string                     { return "" }
func (a *dispAccount) SessionID() string                    { return "" }
func (a *dispAccount) ProxyURL() string                     { return "" }
func (a *dispAccount) IsAPIKey() bool                       { return a.apikey }
func (a *dispAccount) APIKey() string                       { return "sk" }
func (a *dispAccount) LegacyImagesEnabled() bool            { return a.web }
func (a *dispAccount) QuotaSnapshot() *AccountQuotaSnapshot { return nil }

// scriptedDriver returns scripted (result, error) pairs in order.
type scriptedDriver struct {
	name    string
	results []*ImageResult
	errors  []error
	calls   int
}

func (d *scriptedDriver) Name() string { return d.name }
func (d *scriptedDriver) Forward(_ context.Context, _ AccountView, _ *ImagesRequest) (*ImageResult, error) {
	i := d.calls
	d.calls++
	if i >= len(d.errors) {
		return nil, errors.New("scriptedDriver: out of script")
	}
	return d.results[i], d.errors[i]
}

// fakeSource hands out a queue of accounts and records callbacks.
type fakeSource struct {
	mu             sync.Mutex
	accounts       []AccountView
	idx            int
	successCalls   []int64
	rateLimitCalls []rlEntry
	transientCalls []int64
	authCalls      []int64
	releaseCount   int
	selectErr      error
}

type rlEntry struct {
	ID      int64
	ResetAt time.Time
}

func (s *fakeSource) Select(_ context.Context, _ PoolFilter) (AccountView, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.selectErr != nil {
		return nil, nil, s.selectErr
	}
	if s.idx >= len(s.accounts) {
		return nil, nil, errors.New("no more accounts")
	}
	a := s.accounts[s.idx]
	s.idx++
	return a, func() {
		s.mu.Lock()
		s.releaseCount++
		s.mu.Unlock()
	}, nil
}

func (s *fakeSource) OnSuccess(_ context.Context, a AccountView, _ *ImageResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.successCalls = append(s.successCalls, a.ID())
	return nil
}
func (s *fakeSource) OnRateLimit(_ context.Context, a AccountView, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimitCalls = append(s.rateLimitCalls, rlEntry{a.ID(), t})
	return nil
}
func (s *fakeSource) OnTransient(_ context.Context, a AccountView, _ error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transientCalls = append(s.transientCalls, a.ID())
	return nil
}
func (s *fakeSource) OnAuthFailure(_ context.Context, a AccountView, _ error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authCalls = append(s.authCalls, a.ID())
	return nil
}

func newRegistry(name string, d Driver) DriverRegistry {
	return MapDriverRegistry{name: d}
}

func defaultInput() DispatchInput {
	return DispatchInput{
		Capability: Capability{DriverName: DriverAPIKey, Plan: "basic"},
		Filter:     PoolFilter{},
		Request:    &ImagesRequest{Prompt: "x"},
	}
}

func TestDispatch_HappyFirstTry(t *testing.T) {
	src := &fakeSource{accounts: []AccountView{&dispAccount{id: 1, apikey: true}}}
	drv := &scriptedDriver{
		name:    "apikey",
		results: []*ImageResult{{Items: []ImageItem{{B64JSON: "AAA"}}}},
		errors:  []error{nil},
	}
	out, err := Dispatch(context.Background(), src, newRegistry("apikey", drv), defaultInput(), DispatchOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Attempts != 1 || out.DriverUsed != "apikey" {
		t.Errorf("dispatch result: %+v", out)
	}
	if len(src.successCalls) != 1 || src.successCalls[0] != 1 {
		t.Errorf("success cb: %v", src.successCalls)
	}
	if src.releaseCount != 1 {
		t.Errorf("release count = %d", src.releaseCount)
	}
}

func TestDispatch_RateLimitThenSucceed(t *testing.T) {
	src := &fakeSource{accounts: []AccountView{
		&dispAccount{id: 1, apikey: true},
		&dispAccount{id: 2, apikey: true},
	}}
	drv := &scriptedDriver{
		name: "apikey",
		results: []*ImageResult{nil, {Items: []ImageItem{{B64JSON: "OK"}}}},
		errors: []error{
			&RateLimitError{ResetAfter: 90 * time.Second, Reason: "throttled"},
			nil,
		},
	}
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	out, err := Dispatch(context.Background(), src, newRegistry("apikey", drv), defaultInput(),
		DispatchOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Attempts != 2 || out.Account.ID() != 2 {
		t.Errorf("expected 2nd account on 2nd attempt: %+v", out)
	}
	if len(src.rateLimitCalls) != 1 || src.rateLimitCalls[0].ID != 1 {
		t.Errorf("rate limit cb: %+v", src.rateLimitCalls)
	}
	wantReset := now.Add(90 * time.Second)
	if !src.rateLimitCalls[0].ResetAt.Equal(wantReset) {
		t.Errorf("reset_at=%v want=%v", src.rateLimitCalls[0].ResetAt, wantReset)
	}
	if len(src.successCalls) != 1 || src.successCalls[0] != 2 {
		t.Errorf("success: %v", src.successCalls)
	}
	if src.releaseCount != 2 {
		t.Errorf("release count: %d", src.releaseCount)
	}
}

func TestDispatch_RateLimitNoResetUsesDefault(t *testing.T) {
	src := &fakeSource{accounts: []AccountView{
		&dispAccount{id: 1, apikey: true},
		&dispAccount{id: 2, apikey: true},
	}}
	drv := &scriptedDriver{
		name:    "apikey",
		results: []*ImageResult{nil, {Items: []ImageItem{{B64JSON: "OK"}}}},
		errors:  []error{&RateLimitError{}, nil},
	}
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	_, err := Dispatch(context.Background(), src, newRegistry("apikey", drv), defaultInput(),
		DispatchOptions{Now: func() time.Time { return now }, DefaultRateLimitCooldown: 7 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !src.rateLimitCalls[0].ResetAt.Equal(now.Add(7 * time.Minute)) {
		t.Errorf("default cooldown not applied: %v", src.rateLimitCalls[0].ResetAt)
	}
}

func TestDispatch_AuthFailureMarksCooldown(t *testing.T) {
	src := &fakeSource{accounts: []AccountView{
		&dispAccount{id: 1, apikey: true},
		&dispAccount{id: 2, apikey: true},
	}}
	drv := &scriptedDriver{
		name:    "apikey",
		results: []*ImageResult{nil, {Items: []ImageItem{{B64JSON: "OK"}}}},
		errors:  []error{&AuthError{HTTPStatus: 401, Reason: "invalid"}, nil},
	}
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	out, err := Dispatch(context.Background(), src, newRegistry("apikey", drv), defaultInput(),
		DispatchOptions{Now: func() time.Time { return now }, AuthCooldown: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if out.Attempts != 2 {
		t.Errorf("attempts=%d", out.Attempts)
	}
	if len(src.authCalls) != 1 || src.authCalls[0] != 1 {
		t.Errorf("auth callbacks: %v", src.authCalls)
	}
	// auth failure should also schedule a cooldown
	if len(src.rateLimitCalls) != 1 || !src.rateLimitCalls[0].ResetAt.Equal(now.Add(30*time.Minute)) {
		t.Errorf("auth cooldown scheduling: %+v", src.rateLimitCalls)
	}
}

func TestDispatch_TransientRetriesWithBackoff(t *testing.T) {
	src := &fakeSource{accounts: []AccountView{
		&dispAccount{id: 1, apikey: true},
		&dispAccount{id: 2, apikey: true},
		&dispAccount{id: 3, apikey: true},
	}}
	drv := &scriptedDriver{
		name:    "apikey",
		results: []*ImageResult{nil, nil, {Items: []ImageItem{{B64JSON: "OK"}}}},
		errors: []error{
			&TransportError{HTTPStatus: 500, Reason: "boom"},
			&TransportError{HTTPStatus: 502, Reason: "again"},
			nil,
		},
	}
	var sleeps []time.Duration
	out, err := Dispatch(context.Background(), src, newRegistry("apikey", drv), defaultInput(),
		DispatchOptions{Sleep: func(d time.Duration) { sleeps = append(sleeps, d) }})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Attempts != 3 {
		t.Errorf("attempts=%d", out.Attempts)
	}
	if len(src.transientCalls) != 2 {
		t.Errorf("transient cb: %v", src.transientCalls)
	}
	if len(sleeps) != 2 {
		t.Errorf("sleeps: %v", sleeps)
	}
	if sleeps[0] >= sleeps[1] {
		t.Errorf("backoff should grow: %v", sleeps)
	}
}

func TestDispatch_FatalUpstreamErrorReturnsImmediately(t *testing.T) {
	src := &fakeSource{accounts: []AccountView{
		&dispAccount{id: 1, apikey: true},
		&dispAccount{id: 2, apikey: true},
	}}
	drv := &scriptedDriver{
		name:    "apikey",
		results: []*ImageResult{nil},
		errors:  []error{&UpstreamError{HTTPStatus: 400, Reason: "bad prompt"}},
	}
	_, err := Dispatch(context.Background(), src, newRegistry("apikey", drv), defaultInput(), DispatchOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Errorf("expect UpstreamError, got %T", err)
	}
	// Should not have selected 2nd account.
	if drv.calls != 1 {
		t.Errorf("driver called %d times, expected 1", drv.calls)
	}
}

func TestDispatch_MaxAttemptsExceeded(t *testing.T) {
	src := &fakeSource{accounts: []AccountView{
		&dispAccount{id: 1, apikey: true}, &dispAccount{id: 2, apikey: true},
	}}
	drv := &scriptedDriver{
		name:    "apikey",
		results: []*ImageResult{nil, nil},
		errors: []error{
			&RateLimitError{ResetAfter: time.Minute},
			&RateLimitError{ResetAfter: time.Minute},
		},
	}
	_, err := Dispatch(context.Background(), src, newRegistry("apikey", drv), defaultInput(),
		DispatchOptions{MaxAttempts: 2})
	if !errors.Is(err, ErrMaxAttemptsExceeded) {
		t.Errorf("expected ErrMaxAttemptsExceeded, got %v", err)
	}
}

func TestDispatch_SelectFailureReturnsNoAccount(t *testing.T) {
	src := &fakeSource{selectErr: errors.New("no candidates")}
	drv := &scriptedDriver{name: "apikey"}
	_, err := Dispatch(context.Background(), src, newRegistry("apikey", drv), defaultInput(), DispatchOptions{})
	if !errors.Is(err, ErrNoAccountAvailable) {
		t.Errorf("expected ErrNoAccountAvailable, got %v", err)
	}
}

func TestDispatch_DriverNotRegistered(t *testing.T) {
	src := &fakeSource{accounts: []AccountView{&dispAccount{id: 1, apikey: true}}}
	_, err := Dispatch(context.Background(), src, MapDriverRegistry{}, defaultInput(), DispatchOptions{})
	if !errors.Is(err, ErrDriverNotRegistered) {
		t.Errorf("expected ErrDriverNotRegistered, got %v", err)
	}
}

func TestDispatch_ContextCancelled(t *testing.T) {
	src := &fakeSource{accounts: []AccountView{&dispAccount{id: 1, apikey: true}}}
	drv := &scriptedDriver{name: "apikey"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Dispatch(ctx, src, newRegistry("apikey", drv), defaultInput(), DispatchOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
