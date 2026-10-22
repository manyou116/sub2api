//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func slowOutputAccount(t *testing.T) (*OpenAIGatewayService, *capacityStoreStub, *Account) {
	t.Helper()
	svc, store := capacityGateway(t, false)
	v := DefaultOpenAICapacityCooldownSettings()
	v.SlowFirstOutputEnabled = true
	require.NoError(t, svc.settingService.SetOpenAICapacityCooldownSettings(context.Background(), v))
	selection := &AccountSelectionResult{Account: &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}}
	require.True(t, svc.admitOpenAICapacity(svc.withOpenAICapacityCooldown(context.Background(), false), selection, "gpt-6.1-sol"))
	require.NotNil(t, selection.Account.capacityAttempt, "latency protection works with overload protection off")
	return svc, store, selection.Account
}

func slowOutputObserver(t *testing.T, ctx context.Context, account *Account, threshold time.Duration) *openAISlowFirstOutput {
	t.Helper()
	ctx = context.WithValue(ctx, openAISlowFirstOutputContextKey{}, openAISlowFirstOutputMetadata{threshold})
	req := httptest.NewRequest(http.MethodPost, "/responses", nil).WithContext(ctx)
	o := startOpenAISlowFirstOutput(req, account)
	require.NotNil(t, o)
	t.Cleanup(func() { o.finish(false) })
	return o
}

func slowOutputWait(t *testing.T, store *capacityStoreStub, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.outcomes) == want
	}, time.Second, time.Millisecond)
}

func TestSlowFirstOutputMetadataThresholdsAndExcludedPaths(t *testing.T) {
	_, _, account := slowOutputAccount(t)
	for _, tc := range []struct {
		body string
		want time.Duration
	}{
		{`{"stream":true,"reasoning":{"effort":"medium"}}`, 10 * time.Second},
		{`{"stream":true,"reasoning":{"effort":"high"}}`, 30 * time.Second},
		{`{"stream":true,"reasoning":{"effort":"xhigh"}}`, 30 * time.Second},
		{`{"stream":true,"reasoning":{"effort":"max"}}`, 30 * time.Second},
		{`{"stream":false}`, 0},
		{`{"stream":true,"tools":[{"type":"image_generation"}]}`, 0},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		req = withOpenAISlowFirstOutputMetadata(req, account, []byte(tc.body))
		metadata, _ := req.Context().Value(openAISlowFirstOutputContextKey{}).(openAISlowFirstOutputMetadata)
		require.Equal(t, tc.want, metadata.threshold, tc.body)
	}
	account.capacityAttempt.settings.SlowFirstOutputEnabled = false
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	require.Same(t, req, withOpenAISlowFirstOutputMetadata(req, account, []byte(`{"stream":true}`)))
	require.Nil(t, startOpenAISlowFirstOutput(req, account))
}

func TestSlowFirstOutputMeaningfulContentAndBytePreservation(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.output_text.delta","delta":"hello"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`,
		`{"type":"response.function_call_arguments.delta","delta":"{}"}`,
		`{"type":"response.output_item.added","item":{"type":"function_call","arguments":"{}"}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			_, store, account := slowOutputAccount(t)
			o := slowOutputObserver(t, context.Background(), account, time.Hour)
			stream := ": heartbeat\r\n\r\nevent: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\r\n\r\ndata: " + payload + "\r\n\r\n"
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
			o.wrapResponse(resp, nil)
			var got strings.Builder
			buf := make([]byte, 1) // Every SSE line and CRLF crosses read boundaries.
			for {
				n, err := resp.Body.Read(buf)
				_, _ = got.Write(buf[:n])
				if err != nil {
					require.ErrorIs(t, err, io.EOF)
					break
				}
			}
			require.NoError(t, resp.Body.Close())
			require.Equal(t, stream, got.String())
			require.True(t, openAISlowFirstOutputHealthy(account))
			require.False(t, openAISlowFirstOutputOwnsTimeout(account), "post-output gaps retain existing timeout policy")
			observeOpenAICapacityResult(account, true)
			require.Equal(t, []int{2}, store.outcomes)
		})
	}
}

func TestSlowFirstOutputDeadlineCountsBeforeCompletionAndDeduplicatesRetries(t *testing.T) {
	_, store, account := slowOutputAccount(t)
	o := slowOutputObserver(t, context.Background(), account, 20*time.Millisecond)
	slowOutputWait(t, store, 1)
	store.mu.Lock()
	require.Equal(t, []int{3}, store.outcomes)
	store.mu.Unlock()
	require.False(t, openAISlowFirstOutputHealthy(account))
	o.finish(true) // Eventually successful, but still slow.
	observeOpenAICapacityResult(account, true)
	retry := slowOutputObserver(t, context.Background(), account, time.Hour)
	retry.mu.Lock()
	retry.start = time.Now().Add(-2 * time.Hour)
	retry.mu.Unlock()
	retry.expired()
	retry.finish(true)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, []int{3}, store.outcomes, "late success cannot erase the slow event or release its pending probe; retries count once")
}

func TestSlowFirstOutputCancellationStopsObservation(t *testing.T) {
	_, store, account := slowOutputAccount(t)
	ctx, cancel := context.WithCancel(context.Background())
	account.capacityAttempt.ctx = ctx
	o := slowOutputObserver(t, ctx, account, time.Hour)
	cancel()
	o.mu.Lock()
	o.start = time.Now().Add(-2 * time.Hour)
	o.mu.Unlock()
	o.expired()
	o.finish(true)
	require.Empty(t, store.outcomes)
	require.False(t, openAISlowFirstOutputHealthy(account))
}

func TestSlowFirstOutputNoContentAndUnmeasuredSuccessCannotRecover(t *testing.T) {
	for _, stream := range []string{
		": ping\n\ndata: {\"type\":\"response.created\"}\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":{}}\n\n",
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\"}}}\n\n",
		"data: " + strings.Repeat("x", 65*1024) + "\n\n",
	} {
		_, store, account := slowOutputAccount(t)
		o := slowOutputObserver(t, context.Background(), account, time.Hour)
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
		o.wrapResponse(resp, nil)
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, stream, string(got))
		require.False(t, openAISlowFirstOutputHealthy(account))
		observeOpenAICapacityResult(account, true)
		require.Equal(t, []int{0}, store.outcomes)
	}
}

type slowFirstOutputHTTPUpstream struct{ client *http.Client }

func (u slowFirstOutputHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func (u slowFirstOutputHTTPUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func TestSlowFirstOutputNativeAndPassthroughDispatchObserveHeadersAndHeartbeats(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, headersFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%v/headers_first=%v", passthrough, headersFirst), func(t *testing.T) {
				svc, store, account := slowOutputAccount(t)
				svc.cfg = rawChatCompletionsTestConfig()
				account.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
				release := make(chan struct{})
				var releaseOnce sync.Once
				releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
				stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}" // EOF without trailing newline.
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if headersFirst {
						_, _ = io.WriteString(w, ": heartbeat\n\ndata: {\"type\":\"response.created\"}\n\n")
						if flusher, ok := w.(http.Flusher); ok {
							flusher.Flush()
						}
					}
					select {
					case <-release:
						_, _ = io.WriteString(w, stream)
					case <-r.Context().Done():
					}
				}))
				defer server.Close()
				defer releaseUpstream()
				svc.httpUpstream = slowFirstOutputHTTPUpstream{server.Client()}
				body := []byte(`{"model":"gpt-6.1-sol","stream":true,"input":"hello"}`)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
				var req *http.Request
				var err error
				if passthrough {
					req, err = svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "test-token")
				} else {
					req, err = svc.buildUpstreamRequest(context.Background(), c, account, body, "test-token", true, "", false)
				}
				require.NoError(t, err)
				metadata, ok := req.Context().Value(openAISlowFirstOutputContextKey{}).(openAISlowFirstOutputMetadata)
				require.True(t, ok)
				require.Equal(t, 10*time.Second, metadata.threshold)
				req = req.WithContext(context.WithValue(req.Context(), openAISlowFirstOutputContextKey{}, openAISlowFirstOutputMetadata{20 * time.Millisecond}))
				req.URL, err = url.Parse(server.URL)
				require.NoError(t, err)
				type result struct {
					body []byte
					err  error
				}
				done := make(chan result, 1)
				go func() {
					resp, err := svc.doOpenAIUpstream(req, "", account)
					if err != nil {
						done <- result{err: err}
						return
					}
					data, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					done <- result{data, err}
				}()
				slowOutputWait(t, store, 1) // Count before upstream headers/content or completion.
				releaseUpstream()
				select {
				case got := <-done:
					require.NoError(t, got.err)
					require.True(t, strings.HasSuffix(string(got.body), stream), "original bytes must reach the caller")
				case <-time.After(time.Second):
					t.Fatal("latency protection interrupted the existing request")
				}
				observeOpenAICapacityResult(account, true)
				store.mu.Lock()
				require.Equal(t, []int{3}, store.outcomes)
				store.mu.Unlock()
			})
		}
	}
}

func TestSlowFirstOutputOwnsOnlyPreOutputTimeout(t *testing.T) {
	svc, store, account := slowOutputAccount(t)
	o := slowOutputObserver(t, context.Background(), account, time.Hour)
	o.mu.Lock()
	o.start = time.Now().Add(-2 * time.Hour)
	o.mu.Unlock()
	require.False(t, svc.rateLimitService.HandleStreamTimeout(context.Background(), account, "gpt-6.1-sol"))
	slowOutputWait(t, store, 1)
	require.True(t, openAISlowFirstOutputOwnsTimeout(account))
	o.finish(true)
	require.False(t, openAISlowFirstOutputOwnsTimeout(account))
}

func TestSlowFirstOutputPendingProbeSettlesBeforeCancellationReleasesLease(t *testing.T) {
	svc, store, _ := slowOutputAccount(t)
	store.decision.Probe = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selection := &AccountSelectionResult{Account: &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}}
	require.True(t, svc.admitOpenAICapacity(svc.withOpenAICapacityCooldown(ctx, false), selection, "gpt-6.1-sol"))
	a := selection.Account.capacityAttempt
	pending := make(chan struct{})
	a.slowMu.Lock()
	a.slowRecordDone = pending
	a.slowMu.Unlock()
	cancel()
	select {
	case <-store.released:
		t.Fatal("cancellation raced ahead of a pending slow-probe result")
	case <-time.After(20 * time.Millisecond):
	}
	close(pending)
	select {
	case <-store.released:
	case <-time.After(time.Second):
		t.Fatal("probe was not released after its pending result settled")
	}
}

func TestSlowFirstOutputDoesNotSuppressIndependentOverloadObservation(t *testing.T) {
	svc, store, account := slowOutputAccount(t)
	account.capacityAttempt.settings.Enabled = true
	o := slowOutputObserver(t, context.Background(), account, 20*time.Millisecond)
	slowOutputWait(t, store, 1)
	observeOpenAICapacity(account, 1)
	o.finish(false)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, []int{3, 1}, store.outcomes)
	require.True(t, svc.shouldSwitchOpenAICapacity(account, []byte(`{"error":{"code":"server_is_overloaded"}}`)))
}

func TestSlowFirstOutputPauseReasonAndWebException(t *testing.T) {
	svc, store, account := slowOutputAccount(t)
	account.Status, account.Schedulable = StatusActive, true
	repo := &capacityPauseRepo{rateLimitClearRepoStub: rateLimitClearRepoStub{getByIDAccount: account}}
	svc.rateLimitService.accountRepo = repo
	store.trip = OpenAICapacityDecision{Tripped: true, Generation: 8, UntilMS: time.Now().Add(time.Minute).UnixMilli(), Failures: 2, Level: 1}
	store.current = store.trip
	recordOpenAITextCooldown(account, 3, &openAISlowFirstOutputObservation{10001 * time.Millisecond, 10 * time.Second})
	require.Equal(t, 1, repo.setCalls)
	require.False(t, account.IsSchedulable())
	require.True(t, account.IsSchedulableIgnoringTextRateLimit())
	state, err := svc.rateLimitService.GetTempUnschedStatus(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "openai_slow_first_output", state.MatchedKeyword)
	require.EqualValues(t, 2, state.TriggerCount)
	require.Equal(t, 2, state.TriggerThreshold)
	require.Zero(t, state.StatusCode)
	require.Contains(t, state.ErrorMessage, "10001 毫秒")
	require.Contains(t, state.ErrorMessage, "首字慢")
	require.Contains(t, state.ErrorMessage, "Web 生图除外")
}
