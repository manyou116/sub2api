package webdriver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func protocolPNG(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	return out.Bytes()
}

func protocolFaultDriver(roundTrip func(*http.Request) (*http.Response, error)) *Driver {
	d := NewDriver(func(string) (*req.Client, error) {
		client := req.C()
		client.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc { return roundTrip })
		return client, nil
	})
	d.KeepConversation = true
	return d
}

func protocolResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestGenerationSubmissionBoundary(t *testing.T) {
	for _, failure := range []string{"requirements", "submit", "rejected", "missing-cid", "download"} {
		t.Run(failure, func(t *testing.T) {
			cause := errors.New("connection reset by peer")
			submissions, downloads := 0, 0
			d := protocolFaultDriver(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/backend-api/sentinel/chat-requirements/prepare":
					if failure == "requirements" {
						return nil, cause
					}
					return protocolResponse(200, `{"prepare_token":"p"}`), nil
				case "/backend-api/sentinel/chat-requirements/finalize":
					return protocolResponse(200, `{"token":"t"}`), nil
				case "/backend-api/f/conversation/prepare":
					return protocolResponse(200, `{}`), nil
				case "/backend-api/f/conversation":
					submissions++
					if failure == "submit" {
						return nil, cause
					}
					if failure == "rejected" {
						return protocolResponse(429, `{"error":{"message":"Too many requests"}}`), nil
					}
					if failure == "missing-cid" {
						return protocolResponse(200, ""), nil
					}
					return protocolResponse(200, "data: {\"conversation_id\":\"cid\",\"asset_pointer\":\"file-service://file-output\"}\n\n"), nil
				case "/backend-api/files/download/file-output":
					downloads++
					return nil, cause
				default:
					return nil, fmt.Errorf("unexpected path: %s", r.URL.Path)
				}
			})
			_, err := d.Generate(context.Background(), Auth{AccessToken: "local"}, GenerateRequest{Prompt: "draw", N: 1})
			var typed *Error
			require.ErrorAs(t, err, &typed)
			require.Equal(t, failure != "requirements" && failure != "rejected", typed.MayHaveGenerated())
			if failure == "submit" {
				require.Equal(t, SubmissionUncertain, typed.Submission)
			}
			if failure == "download" {
				require.Equal(t, "cid", typed.ConversationID)
				require.Equal(t, 6, downloads)
			}
			if failure == "requirements" || failure == "submit" || failure == "download" {
				require.ErrorIs(t, err, cause)
			}
			if failure == "requirements" {
				require.Zero(t, submissions)
			} else {
				require.Equal(t, 1, submissions)
			}
		})
	}
}

func TestRetryAfterAnd403Classification(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"12", 12 * time.Second}, {now.Add(time.Minute).Format(http.TimeFormat), time.Minute}, {"-2", 0}, {"invalid", 0}, {"999999999999", 24 * time.Hour},
	} {
		require.Equal(t, tc.want, parseRetryAfter(tc.raw, now))
	}
	err := classifyHTTP("probe", 429, `{"error":{"code":"rate_limit_exceeded"}}`, http.Header{"Retry-After": []string{"42"}})
	require.Equal(t, 42*time.Second, err.RetryAfter)
	require.Equal(t, ErrorKindAuth, classifyHTTP("probe", 403, `{"error":{"code":"token_invalidated"}}`).Kind)
	require.Equal(t, ErrorKindUpstream, classifyHTTP("probe", 403, `{"error":{"code":"permission_denied"}}`).Kind)
	for _, text := range []string{"这次图片生成违反了内容政策", "不能完成生图", "image generation unavailable", "rate limit for text requests", "image generation rate limit exceeded"} {
		require.False(t, IsImageQuotaLimitedMessage(text))
	}
	for _, text := range []string{"image_gen_quota_exceeded", "You've hit the Plus plan limit for image generation", "图片生成次数已用完"} {
		require.True(t, IsImageQuotaLimitedMessage(text))
	}
}

type trackedSSEReader struct{ reads atomic.Int64 }

func (r *trackedSSEReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return copy(p, strings.Repeat("data: {}\n", 100)), nil
}

func TestSSEReaderCancellationUnblocksFullChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &trackedSSEReader{}
	lines, _ := readSSELines(ctx, r)
	require.Eventually(t, func() bool { return len(lines) == 64 }, time.Second, time.Millisecond)
	cancel()
	done := make(chan struct{})
	go func() {
		for range lines {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader leaked after cancellation")
	}
}

func TestDownloadBodyLimitsAndContentValidation(t *testing.T) {
	response := protocolResponse(200, strings.Repeat("x", maxDownloadedImageBytes+1))
	_, err := readDownloadBody(&req.Response{Response: response})
	require.ErrorContains(t, err, "size limit")
	require.NoError(t, validateDownloadedImage(protocolPNG(t)))
	require.ErrorContains(t, validateDownloadedImage([]byte("<html>blocked</html>")), "invalid image")
	for _, raw := range []string{"https://chatgpt.com/file", "https://files.chatgpt.com/file"} {
		require.True(t, chatGPTDownloadURL(raw))
	}
	for _, raw := range []string{"https://chatgpt.com.evil.invalid/file", "https://evil.invalid/chatgpt.com/file", "http://chatgpt.com/file", "https://user@chatgpt.com/file"} {
		require.False(t, chatGPTDownloadURL(raw))
	}
}

func TestDownloadTransientReadRetriesSameAsset(t *testing.T) {
	calls := 0
	data, err := retryDownload(context.Background(), 3, time.Millisecond, func() ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, transportError("download", io.ErrUnexpectedEOF)
		}
		return protocolPNG(t), nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.NoError(t, validateDownloadedImage(data))
	calls = 0
	_, err = retryDownload(context.Background(), 3, time.Millisecond, func() ([]byte, error) {
		calls++
		return nil, NewError(ErrorKindAuth, "download", "token not found", 401, false)
	})
	require.Error(t, err)
	require.Equal(t, 1, calls, "missing auth must not be mistaken for a pending file")
}

func TestDownloadResolverRespectsThrottleBeforeTryingAlternateRoutes(t *testing.T) {
	calls := 0
	d := protocolFaultDriver(func(*http.Request) (*http.Response, error) {
		calls++
		resp := protocolResponse(429, `{"error":{"message":"Too many requests"}}`)
		resp.Header.Set("Retry-After", "60")
		return resp, nil
	})
	client, headers, err := d.session(Auth{AccessToken: "local"})
	require.NoError(t, err)
	_, err = d.downloadOne(context.Background(), client, headers, "cid", "file-output", false)
	var typed *Error
	require.ErrorAs(t, err, &typed)
	require.Equal(t, time.Minute, typed.RetryAfter)
	require.Equal(t, 1, calls, "429 must not cascade immediately through alternate download routes")
}

func TestPollRetryAfterOutlivesReadBudgetWithoutAnotherGET(t *testing.T) {
	calls := 0
	d := protocolFaultDriver(func(*http.Request) (*http.Response, error) {
		calls++
		resp := protocolResponse(429, `{"error":{"message":"Too many requests"}}`)
		resp.Header.Set("Retry-After", "60")
		return resp, nil
	})
	d.PollTimeout = 50 * time.Millisecond
	d.PollInterval = time.Millisecond
	d.PollInitialWait = 0
	auth := Auth{AccessToken: t.Name()}
	client, headers, err := d.session(auth)
	require.NoError(t, err)
	_, _, _, err = d.pollImages(context.Background(), client, headers, auth, "cid", nil, false, "", false)
	var typed *Error
	require.ErrorAs(t, err, &typed)
	require.Equal(t, ErrorKindRateLimited, typed.Kind)
	require.Greater(t, typed.RetryAfter, 59*time.Second)
	require.Equal(t, 1, calls)
}

func TestStageLogsRedactCredentials(t *testing.T) {
	raw := "proxy socks5h://user:pass@proxy.invalid:1080 authentication failed; Bearer abc-secret; access_token=token-secret; eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signature\nnext"
	clean := safeStageLogMessage(raw)
	for _, secret := range []string{"user", "pass@", "abc-secret", "token-secret", "eyJhbG", "\n"} {
		require.NotContains(t, clean, secret)
	}
	require.Contains(t, clean, "authentication failed")
}
