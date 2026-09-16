package proxybudget

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFingerprintMatchesFrozenContract(t *testing.T) {
	fingerprint, err := Fingerprint("socks5://User:p%40ss@EXAMPLE.com:1080/path?ignored=yes")
	require.NoError(t, err)
	require.Equal(t, "33bd2537fc9117e42d07a569be337c45cda22a040d8157f27b5f6d1071341f59", fingerprint)
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

// jsonNewDecoder keeps this test's intent readable without exposing transport
// helpers from the package API.
func jsonNewDecoder(r io.Reader) *json.Decoder { return json.NewDecoder(r) }
