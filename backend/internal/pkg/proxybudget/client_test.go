package proxybudget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestFingerprintMatchesFrozenContract(t *testing.T) {
	fingerprint, err := Fingerprint("socks5://User:p%40ss@EXAMPLE.com:1080/path?ignored=yes")
	require.NoError(t, err)
	require.Equal(t, "33bd2537fc9117e42d07a569be337c45cda22a040d8157f27b5f6d1071341f59", fingerprint)
}

func TestFingerprintUsesCompactUTF8AndZeroForMissingPort(t *testing.T) {
	fingerprint, err := Fingerprint("http://a%26%3C%3E%E2%80%A8:p%E2%80%A9@EXAMPLE.com")
	require.NoError(t, err)
	require.Equal(t, "2a412cabf32317fbc25c4633f7cd472c5666293a93017c0de8382e4fb33a5850", fingerprint)
}

func TestNewDisabledOnlyWhenBothEnvironmentValuesAbsent(t *testing.T) {
	require.True(t, New("", "", nil).Disabled())
	require.True(t, New("http://127.0.0.1:8080", "", nil).Invalid())
	require.True(t, New("http://budget.example", "token", nil).Invalid())
}

func TestReserveRetryUsesSameLeaseSequence(t *testing.T) {
	var calls atomic.Int64
	var first, second reserveRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if r.URL.Path == "/reserve" {
			var input reserveRequest
			require.NoError(t, jsonNewDecoder(r.Body).Decode(&input))
			if calls.Add(1) == 1 {
				first = input
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			second = input
			_, _ = w.Write([]byte(`{"allowed":true,"managed":true,"mode":"enforce","granted_bytes":69632}`))
			return
		}
		_, _ = w.Write([]byte(`{"settled":true}`))
	}))
	defer server.Close()

	lease, err := New(server.URL, "secret", server.Client()).OpenLease(context.Background(), "http://proxy.example:8080")
	require.NoError(t, err)
	require.Equal(t, int64(2), calls.Load())
	require.Equal(t, first.LeaseID, second.LeaseID)
	require.Equal(t, first.Sequence, second.Sequence)
	require.Equal(t, first.Bytes, second.Bytes)
	require.NoError(t, lease.Settle(context.Background()))
}

func TestLeaseDenialAndSettlementAreIdempotent(t *testing.T) {
	var settles atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/reserve" {
			_, _ = w.Write([]byte(`{"allowed":false,"managed":true,"mode":"enforce","reason":"daily cap"}`))
			return
		}
		settles.Add(1)
		_, _ = w.Write([]byte(`{"settled":true}`))
	}))
	defer server.Close()

	_, err := New(server.URL, "secret", server.Client()).OpenLease(context.Background(), "http://proxy.example:8080")
	require.ErrorIs(t, err, ErrDenied)
	require.Equal(t, int64(0), settles.Load())
}

func TestShadowLeaseCanObserveBeyondInitialReservation(t *testing.T) {
	var actual atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/reserve" {
			_, _ = w.Write([]byte(`{"allowed":false,"managed":true,"mode":"shadow","granted_bytes":0}`))
			return
		}
		defer r.Body.Close()
		var input settleRequest
		require.NoError(t, jsonNewDecoder(r.Body).Decode(&input))
		actual.Store(input.ActualBytes)
		_, _ = w.Write([]byte(`{"settled":true}`))
	}))
	defer server.Close()

	lease, err := New(server.URL, "secret", server.Client()).OpenLease(context.Background(), "http://proxy.example:8080")
	require.NoError(t, err)
	lease.Observe(200 << 10)
	require.NoError(t, lease.Settle(context.Background()))
	require.NoError(t, lease.Settle(context.Background()))
	require.Equal(t, int64((200<<10)+(4<<10)), actual.Load())
}

func TestIOReservationAndConcurrentSettlementKeepPendingBytes(t *testing.T) {
	var settledActual atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/reserve" {
			_, _ = w.Write([]byte(`{"allowed":true,"managed":true,"mode":"enforce","granted_bytes":262144}`))
			return
		}
		defer r.Body.Close()
		var input settleRequest
		require.NoError(t, jsonNewDecoder(r.Body).Decode(&input))
		settledActual.Store(input.ActualBytes)
		_, _ = w.Write([]byte(`{"settled":true}`))
	}))
	defer server.Close()

	lease, err := New(server.URL, "secret", server.Client()).OpenLease(context.Background(), "http://proxy.example:8080")
	require.NoError(t, err)
	pending, err := lease.ReserveIO(context.Background(), 64<<10)
	require.NoError(t, err)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() { defer workers.Done(); _ = lease.Settle(context.Background()) }()
	}
	workers.Wait()
	pending.Finish(0) // A late completion cannot lower the settled amount.
	require.GreaterOrEqual(t, settledActual.Load(), int64((64<<10)+(4<<10)))
}

func TestLeaseRolloverSettlesOldLeaseAndRetriesWithFreshID(t *testing.T) {
	var leaseIDs []string
	var reserveCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/reserve" {
			defer r.Body.Close()
			var input reserveRequest
			require.NoError(t, jsonNewDecoder(r.Body).Decode(&input))
			leaseIDs = append(leaseIDs, input.LeaseID)
			if reserveCalls.Add(1) == 2 {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"detail":{"code":"lease_rollover_required"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"allowed":true,"managed":true,"mode":"enforce","granted_bytes":262144}`))
			return
		}
		_, _ = w.Write([]byte(`{"settled":true}`))
	}))
	defer server.Close()

	lease, err := New(server.URL, "secret", server.Client()).OpenLease(context.Background(), "http://proxy.example:8080")
	require.NoError(t, err)
	first, err := lease.ReserveIO(context.Background(), 128<<10)
	require.NoError(t, err)
	first.Finish(128 << 10)
	second, err := lease.ReserveIO(context.Background(), 128<<10)
	require.NoError(t, err)
	second.Finish(128 << 10)
	require.GreaterOrEqual(t, len(leaseIDs), 3)
	require.NotEqual(t, leaseIDs[0], leaseIDs[len(leaseIDs)-1])
}

func TestLeaseRolloverWithPendingIOChargesOldGenerationOnce(t *testing.T) {
	var reserveCalls atomic.Int64
	var settles []settleRequest
	var settleMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if r.URL.Path == "/reserve" {
			call := reserveCalls.Add(1)
			if call == 2 {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"detail":{"code":"lease_rollover_required"}}`))
				return
			}
			grant := 262144
			if call == 1 {
				grant = 69632
			}
			_, _ = w.Write([]byte(fmt.Sprintf(`{"allowed":true,"managed":true,"mode":"enforce","granted_bytes":%d}`, grant)))
			return
		}
		var input settleRequest
		require.NoError(t, jsonNewDecoder(r.Body).Decode(&input))
		settleMu.Lock()
		settles = append(settles, input)
		settleMu.Unlock()
		_, _ = w.Write([]byte(`{"settled":true}`))
	}))
	defer server.Close()

	lease, err := New(server.URL, "secret", server.Client()).OpenLease(context.Background(), "http://proxy.example:8080")
	require.NoError(t, err)
	old, err := lease.ReserveIO(context.Background(), 64<<10)
	require.NoError(t, err)
	newIO, err := lease.ReserveIO(context.Background(), 128<<10)
	require.NoError(t, err)
	old.Finish(1) // Late completion belongs to the settled old generation.
	newIO.Finish(2)
	settleMu.Lock()
	defer settleMu.Unlock()
	require.Len(t, settles, 1)
	require.Equal(t, int64((64<<10)+(4<<10)), settles[0].ActualBytes)
}

func TestWrapRoundTripperDenialPreventsBaseCall(t *testing.T) {
	budget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"allowed":false,"managed":true,"mode":"enforce","reason":"daily cap"}`))
	}))
	defer budget.Close()
	var baseCalls atomic.Int64
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		baseCalls.Add(1)
		return nil, errors.New("must not send")
	})
	req, err := http.NewRequest(http.MethodGet, "https://upstream.example", nil)
	require.NoError(t, err)
	_, err = New(budget.URL, "secret", budget.Client()).WrapRoundTripper(base, "http://proxy.example:8080").RoundTrip(req)
	require.ErrorIs(t, err, ErrDenied)
	require.Zero(t, baseCalls.Load())
}

// jsonNewDecoder keeps this test's intent readable without exposing transport
// helpers from the package API.
func jsonNewDecoder(r io.Reader) *json.Decoder { return json.NewDecoder(r) }
