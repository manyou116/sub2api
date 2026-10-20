//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestWebImageErrorHealthScopes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		kind          webdriver.ErrorKind
		status        int
		message       string
		cause         error
		reason        string
		parentBlocked bool
	}{
		{"policy", webdriver.ErrorKindPolicy, 403, "生图违反内容政策", nil, "", false},
		{"permission", webdriver.ErrorKindUpstream, 403, "permission denied", nil, "", false},
		{"edge", webdriver.ErrorKindUpstream, 403, "ChatGPT Web returned an HTML 403 page", nil, "edge_block", false},
		{"transient", webdriver.ErrorKindTransport, 0, "unexpected EOF", io.ErrUnexpectedEOF, "network_error", false},
		{"proxy_down", webdriver.ErrorKindTransport, 0, "dial failed", syscall.ECONNREFUSED, "", true},
		{"throttle", webdriver.ErrorKindRateLimited, 429, "too many requests", nil, "rate_limit", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, parent, shadow, _ := webImageFixture(t)
			err := webdriver.NewError(tc.kind, "requirements", tc.message, tc.status, true)
			err.Cause = tc.cause
			err.RetryAfter = 95 * time.Second
			if tc.name == "edge" {
				err.ResponseBody = []byte("<!doctype html><html>challenge</html>")
			}
			s.observeError(context.Background(), parent, "gpt-5-6-thinking", err)
			require.Empty(t, repo.cooldowns, "transient errors must not invent exhausted image quota")
			require.Equal(t, tc.parentBlocked, parent.TempUnschedulableUntil != nil)
			status, statusErr := s.Status(context.Background(), shadow.ID)
			require.NoError(t, statusErr)
			require.Equal(t, tc.reason, status.CooldownReason)
			require.Nil(t, status.Remaining)
			if tc.reason != "" {
				require.GreaterOrEqual(t, status.RetryAfterSeconds, 94)
			}
		})
	}
}

func TestWebImageExplicitQuotaExhaustionForPaidPlan(t *testing.T) {
	s, repo, parent, shadow, _ := webImageFixture(t)
	parent.Credentials["plan_type"] = "plus"
	err := webdriver.NewError(webdriver.ErrorKindRateLimited, "sse", "You've hit the Plus plan limit for image generations requests.", 429, true)
	reset := time.Now().Add(time.Hour)
	err.ResetAt = &reset
	s.observeError(context.Background(), parent, "gpt-5-6-thinking", err)
	require.WithinDuration(t, reset, repo.cooldowns[parent.ID], time.Millisecond)
	status, e := s.Status(context.Background(), shadow.ID)
	require.NoError(t, e)
	require.Zero(t, *status.Remaining)
}

func TestWebImageAcceptedFailureNeverRequestsFailover(t *testing.T) {
	for _, state := range []webdriver.SubmissionState{webdriver.SubmissionUncertain, webdriver.SubmissionAccepted} {
		t.Run(string(state), func(t *testing.T) {
			s, _, _, shadow, _ := webImageFixture(t)
			s.driver = &fakeWebImageDriver{generate: func(context.Context, webdriver.Auth, webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
				err := webdriver.NewError(webdriver.ErrorKindTransport, "download", "connection reset", 0, true)
				err.Submission = state
				err.ConversationID = "existing-conversation"
				err.Cause = context.DeadlineExceeded
				return nil, err
			}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			_, err := s.gateway.forwardWebImage(context.Background(), c, shadow, &OpenAIImagesRequest{Model: "gpt-image-2", Prompt: "test"}, "")
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			require.Equal(t, 502, recorder.Code)
			require.Contains(t, recorder.Body.String(), "web_image_result_unconfirmed")
		})
	}
}

func TestWebImageClientPoolReusesTransportWithoutCookieState(t *testing.T) {
	pool := newWebImageClientPool()
	a, err := pool.client("")
	require.NoError(t, err)
	b, err := pool.client("")
	require.NoError(t, err)
	require.Same(t, a, b)
	c, err := pool.client("http://127.0.0.1:1234")
	require.NoError(t, err)
	require.NotSame(t, a, c)
	var peers []string
	var peersMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peersMu.Lock()
		peers = append(peers, r.RemoteAddr)
		peersMu.Unlock()
		require.Empty(t, r.Header.Get("Cookie"))
		require.Equal(t, "Bearer "+r.URL.Path[1:], r.Header.Get("Authorization"))
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "previous-identity"})
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	for _, identity := range []string{"first", "second"} {
		response, err := a.R().SetHeader("Authorization", "Bearer "+identity).Get(server.URL + "/" + identity)
		require.NoError(t, err)
		require.Equal(t, "ok", response.String())
	}
	peersMu.Lock()
	defer peersMu.Unlock()
	require.Len(t, peers, 2)
	require.Equal(t, peers[0], peers[1], "keepalive connection should be reused across identities")
}

func TestWebImageRedirectStripsIdentityOutsideSecureChatGPT(t *testing.T) {
	for _, target := range []string{"https://files.example/image", "https://chatgpt.com.evil.test/image", "http://chatgpt.com/image", "https://chatgpt.com/image"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		for _, key := range []string{"Authorization", "Cookie", "ChatGPT-Account-Id", "Oai-Did", "OpenAI-Sentinel-Proof-Token"} {
			request.Header.Set(key, "private")
		}
		require.NoError(t, webImageRedirectPolicy(request, nil))
		if target == "https://chatgpt.com/image" {
			require.Equal(t, "private", request.Header.Get("Authorization"))
		} else {
			require.Empty(t, request.Header)
		}
	}
}

func TestWebImageAcceptedRejectionPreservesKnownReason(t *testing.T) {
	for _, kind := range []webdriver.ErrorKind{webdriver.ErrorKindPolicy, webdriver.ErrorKindRateLimited} {
		s, _, parent, shadow, _ := webImageFixture(t)
		s.driver = &fakeWebImageDriver{generate: func(context.Context, webdriver.Auth, webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
			failure := webdriver.NewError(kind, "sse", "Request rejected", 429, true)
			failure.Code = "image_gen_quota_exceeded"
			failure.Submission = webdriver.SubmissionAccepted
			failure.RetryAfter = time.Minute
			return nil, failure
		}}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
		_, err := s.gateway.forwardWebImage(context.Background(), c, shadow, &OpenAIImagesRequest{Model: "gpt-image-2", Prompt: "test"}, "")
		var failover *UpstreamFailoverError
		require.False(t, errors.As(err, &failover))
		want := 429
		if kind == webdriver.ErrorKindPolicy {
			want = 400
		}
		require.Equal(t, want, recorder.Code)
		require.NotContains(t, recorder.Body.String(), "web_image_result_unconfirmed")
		require.Nil(t, parent.TempUnschedulableUntil)
		if kind == webdriver.ErrorKindRateLimited {
			status, statusErr := s.Status(context.Background(), shadow.ID)
			require.NoError(t, statusErr)
			require.Zero(t, *status.Remaining)
		}
	}
}

func TestWebImageErrorsRedactCredentials(t *testing.T) {
	s, _, parent, _, _ := webImageFixture(t)
	parent.Credentials["access_token"] = "secret-access"
	parent.Credentials["refresh_token"] = "secret-refresh"
	failure := webdriver.NewError(webdriver.ErrorKindTransport, "download", "GET https://user:password@proxy.test/image?token=secret-query: Bearer other-secret secret-access secret-refresh", 0, true)
	s.redactDriverError(failure, parent)
	for _, secret := range []string{"user:password", "secret-query", "other-secret", "secret-access", "secret-refresh"} {
		require.False(t, strings.Contains(failure.Message, secret), failure.Message)
	}
}
