package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func oauthCompressionBody() []byte {
	var body strings.Builder
	_, _ = body.WriteString(`{"model":"gpt-5.6-luna","stream":true,"input":"`)
	for index := range 3000 {
		fmt.Fprintf(&body, "%x repeated context and tool output; ", sha256.Sum256(fmt.Appendf(nil, "%d", index)))
	}
	_, _ = body.WriteString(`"}`)
	return []byte(body.String())
}

func oauthCompressionRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer local-fixture")
	return req
}

func oauthCompressionResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestOpenAIOAuthRequestCompressionWireAndReplay(t *testing.T) {
	plain := oauthCompressionBody()
	request := oauthCompressionRequest(t, plain)
	request.Header.Set("Content-Length", fmt.Sprint(len(plain)))
	request.TransferEncoding = []string{"chunked"}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	upstream := &httpUpstreamRecorder{resp: oauthCompressionResponse(http.StatusOK, "data: [DONE]\n\n")}
	service := &OpenAIGatewayService{httpUpstream: upstream}
	response, err := service.doOpenAIUpstream(request, "socks5://test.invalid:1080", account)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Len(t, upstream.requests, 1)
	got := upstream.lastReq
	require.Equal(t, "zstd", got.Header.Get("Content-Encoding"))
	assert.Empty(t, got.Header.Get("Content-Length"))
	assert.Empty(t, got.TransferEncoding)
	assert.Equal(t, request.Context(), got.Context())
	assert.Equal(t, request.Header.Get("Authorization"), got.Header.Get("Authorization"))
	assert.Equal(t, "socks5://test.invalid:1080", upstream.lastProxyURL)
	assert.Empty(t, request.Header.Get("Content-Encoding"), "do not mutate the original for fallback")
	for range 2 {
		replay, err := got.GetBody()
		require.NoError(t, err)
		wire, err := io.ReadAll(replay)
		require.NoError(t, err)
		require.NoError(t, replay.Close())
		assert.EqualValues(t, len(wire), got.ContentLength)
		assert.Less(t, len(wire), len(plain))
		decoder, err := zstd.NewReader(bytes.NewReader(wire))
		require.NoError(t, err)
		decoded, err := io.ReadAll(decoder)
		decoder.Close()
		require.NoError(t, err)
		assert.Equal(t, plain, decoded)
	}
}

func TestOpenAIOAuthRequestCompressionEligibility(t *testing.T) {
	plain := oauthCompressionBody()
	for _, scenario := range []struct {
		name string
		edit func(*http.Request, *Account)
	}{
		{"disabled", func(_ *http.Request, a *Account) { a.Extra = map[string]any{"openai_request_compression": false} }},
		{"invalid switch", func(_ *http.Request, a *Account) { a.Extra = map[string]any{"openai_request_compression": "true"} }},
		{"API key", func(_ *http.Request, a *Account) { a.Type = AccountTypeAPIKey }},
		{"setup token", func(_ *http.Request, a *Account) { a.Type = AccountTypeSetupToken }},
		{"other platform", func(_ *http.Request, a *Account) { a.Platform = PlatformAnthropic }},
		{"custom host", func(r *http.Request, _ *Account) { r.URL.Host = "chatgpt.com.example.org" }},
		{"custom port", func(r *http.Request, _ *Account) { r.URL.Host = "chatgpt.com:8443" }},
		{"custom Host", func(r *http.Request, _ *Account) { r.Host = "custom.example.org" }},
		{"HTTP", func(r *http.Request, _ *Account) { r.URL.Scheme = "http" }},
		{"compact", func(r *http.Request, _ *Account) { r.URL.Path += "/compact" }},
		{"non POST", func(r *http.Request, _ *Account) { r.Method = http.MethodPut }},
		{"non JSON", func(r *http.Request, _ *Account) { r.Header.Set("Content-Type", "multipart/form-data") }},
		{"already encoded", func(r *http.Request, _ *Account) { r.Header.Set("Content-Encoding", "gzip") }},
		{"non replayable", func(r *http.Request, _ *Account) { r.GetBody = nil }},
		{"oversize", func(r *http.Request, _ *Account) { r.ContentLength = 64<<20 + 1 }},
		{"non stream", func(r *http.Request, _ *Account) {
			body := bytes.Replace(plain, []byte(`"stream":true`), []byte(`"stream":false`), 1)
			*r = *oauthCompressionRequest(t, body)
		}},
		{"read error", func(r *http.Request, _ *Account) {
			r.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("fixture read error") }
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			req := oauthCompressionRequest(t, plain)
			a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			scenario.edit(req, a)
			upstream := &httpUpstreamRecorder{resp: oauthCompressionResponse(http.StatusOK, "ok")}
			s := &OpenAIGatewayService{httpUpstream: upstream}
			response, err := s.doOpenAIUpstream(req, "", a)
			require.NoError(t, err)
			defer func() { require.NoError(t, response.Body.Close()) }()
			require.Len(t, upstream.requests, 1)
			assert.Same(t, req, upstream.lastReq)
		})
	}
}

func TestOpenAIOAuthRequestCompressionThreshold(t *testing.T) {
	for _, size := range []int{128, 1024, 16384, 32768, 65535, 65536, 65537} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			prefix, suffix := `{"stream":true,"input":"`, `"}`
			body := []byte(prefix + strings.Repeat("a", size-len(prefix)-len(suffix)) + suffix)
			req := oauthCompressionRequest(t, body)
			req.Header.Set("Content-Type", "application/problem+json; charset=utf-8")
			a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_request_compression": true}}
			compressed := compressOpenAIOAuthRequest(req, a)
			require.NotNil(t, compressed)
			require.NoError(t, compressed.Body.Close())
		})
	}
}

func TestOpenAIOAuthRequestCompressionFallback(t *testing.T) {
	plain := oauthCompressionBody()
	for _, scenario := range []struct {
		name     string
		status   int
		body     string
		fallback bool
	}{
		{"415", 415, "unsupported encoding", true},
		{"explicit 400", 400, `{"error":{"message":"Unsupported Content-Encoding: zstd"}}`, true},
		{"decode 400", 400, `{"error":{"message":"Unable to decompress zstd Content-Encoding"}}`, true},
		{"ordinary 400", 400, `{"error":{"message":"Invalid input field"}}`, false},
		{"model name mentions zstd", 400, `{"error":{"message":"Unsupported model zstd"}}`, false},
		{"rate limit", 429, `{"error":{"message":"Unsupported Content-Encoding: zstd"}}`, false},
		{"server error", 503, "overloaded", false},
		{"large ordinary error preserved", 400, strings.Repeat("validation error ", 1024), false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			firstBody := &passthroughCloseTrackingReadCloser{Reader: strings.NewReader(scenario.body)}
			first := &http.Response{StatusCode: scenario.status, Header: make(http.Header), Body: firstBody}
			last := oauthCompressionResponse(http.StatusUnsupportedMediaType, "second response must not retry again")
			upstream := &httpUpstreamRecorder{responses: []*http.Response{first, last}}
			s := &OpenAIGatewayService{httpUpstream: upstream}
			req := oauthCompressionRequest(t, plain)
			response, err := s.doOpenAIUpstream(req, "", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth})
			require.NoError(t, err)
			defer func() { require.NoError(t, response.Body.Close()) }()
			if scenario.fallback {
				require.Len(t, upstream.requests, 2)
				assert.Equal(t, "zstd", upstream.requests[0].Header.Get("Content-Encoding"))
				assert.Empty(t, upstream.requests[1].Header.Get("Content-Encoding"))
				assert.True(t, firstBody.closed)
				assert.Equal(t, plain, upstream.lastBody)
				assert.Same(t, last, response)
			} else {
				require.Len(t, upstream.requests, 1)
				assert.Same(t, first, response)
				body, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				assert.Equal(t, scenario.body, string(body))
			}
		})
	}
}

func TestOpenAIOAuthRequestCompressionDoesNotRetryTransportFailure(t *testing.T) {
	upstream := &httpUpstreamRecorder{err: context.Canceled}
	s := &OpenAIGatewayService{httpUpstream: upstream}
	_, err := s.doOpenAIUpstream(oauthCompressionRequest(t, oauthCompressionBody()), "", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth})
	require.ErrorIs(t, err, context.Canceled)
	assert.Len(t, upstream.requests, 1)
}

func TestOpenAIOAuthRequestCompressionSSE(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))}}
	s := &OpenAIGatewayService{httpUpstream: upstream}
	response, err := s.doOpenAIUpstream(oauthCompressionRequest(t, oauthCompressionBody()), "", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth})
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	assert.Empty(t, response.Header.Get("Content-Encoding"))
	assert.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
	assert.Equal(t, http.StatusOK, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Equal(t, "data: {\"type\":\"response.completed\"}\n\n", string(body))
}
