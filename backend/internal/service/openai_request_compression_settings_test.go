package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type requestCompressionSettingRepo struct {
	panelRateLimitSettingRepo
	setErr error
}

func (r *requestCompressionSettingRepo) Set(ctx context.Context, key, value string) error {
	if r.setErr != nil {
		return r.setErr
	}
	return r.panelRateLimitSettingRepo.Set(ctx, key, value)
}

func (r *requestCompressionSettingRepo) SetMultiple(ctx context.Context, values map[string]string) error {
	if r.setErr != nil {
		return r.setErr
	}
	return r.panelRateLimitSettingRepo.SetMultiple(ctx, values)
}

func TestOpenAIRequestCompressionOptionsPersistCacheAndFailure(t *testing.T) {
	ctx := context.Background()
	repo := &requestCompressionSettingRepo{}
	s := &SettingService{settingRepo: repo}
	initial, err := s.GetOpenAIRequestCompressionOptions(ctx)
	require.NoError(t, err)
	require.Equal(t, defaultOpenAIRequestCompressionOptions(), initial)
	_, err = s.GetOpenAIRequestCompressionOptions(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, repo.getValueCalls)
	changed := OpenAIRequestCompressionOptions{false, 16 << 20, false}
	require.NoError(t, s.SetOpenAIRequestCompressionOptions(ctx, nil, changed))
	other := &SettingService{settingRepo: repo}
	got, err := other.GetOpenAIRequestCompressionOptions(ctx)
	require.NoError(t, err)
	require.Equal(t, changed, got)
	repo.setErr = errors.New("write failed")
	require.Error(t, s.SetOpenAIRequestCompressionOptions(ctx, nil, initial))
	got, err = s.GetOpenAIRequestCompressionOptions(ctx)
	require.NoError(t, err)
	require.Equal(t, changed, got)
	repo.getValueErr = errors.New("database failed")
	other.openAIRequestCompressionOptionsCache.Store(&cachedOpenAIRequestCompressionOptions{until: time.Now().Add(-time.Second)})
	got, err = other.GetOpenAIRequestCompressionOptions(ctx)
	require.Error(t, err)
	require.True(t, got.ImageGuardEnabled)
	require.Equal(t, 8<<20, got.ImageGuardMaxBytes)
	require.False(t, got.Bare400FallbackEnabled)
}

func TestOpenAIRequestCompressionOptionsChangeForwarding(t *testing.T) {
	repo := &requestCompressionSettingRepo{}
	settings := &SettingService{settingRepo: repo}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	upstream := &httpUpstreamRecorder{}
	s := &OpenAIGatewayService{settingService: settings, httpUpstream: upstream}
	body := []byte(`{"stream":true,"input":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("A", (8<<20)) + `"}]}`)
	options := defaultOpenAIRequestCompressionOptions()
	for _, scenario := range []struct {
		guard    bool
		max      int
		encoding string
	}{
		{true, 8 << 20, ""}, {false, 8 << 20, "zstd"}, {true, 16 << 20, "zstd"},
	} {
		options.ImageGuardEnabled, options.ImageGuardMaxBytes = scenario.guard, scenario.max
		require.NoError(t, settings.SetOpenAIRequestCompressionOptions(context.Background(), nil, options))
		upstream.resp = oauthCompressionResponse(200, "ok")
		resp, err := s.doOpenAIUpstream(oauthCompressionRequest(t, body), "", account)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, scenario.encoding, upstream.lastReq.Header.Get("Content-Encoding"))
	}
	for _, allow := range []bool{false, true} {
		options.Bare400FallbackEnabled = allow
		require.NoError(t, settings.SetOpenAIRequestCompressionOptions(context.Background(), nil, options))
		upstream.requests = nil
		upstream.responses = []*http.Response{oauthCompressionResponse(400, `{"detail":"Bad Request"}`), oauthCompressionResponse(200, "ok")}
		resp, err := s.doOpenAIUpstream(oauthCompressionRequest(t, oauthCompressionBody()), "", account)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		if allow {
			require.Len(t, upstream.requests, 2)
			require.Equal(t, 200, resp.StatusCode)
		} else {
			require.Len(t, upstream.requests, 1)
			require.Equal(t, 400, resp.StatusCode)
		}
	}
}

func TestOpenAIRequestCompressionSettings(t *testing.T) {
	ctx := context.Background()
	repo := &requestCompressionSettingRepo{}
	s := &SettingService{settingRepo: repo}
	for range 3 {
		enabled, err := s.GetOpenAIRequestCompression(ctx)
		require.NoError(t, err)
		require.True(t, enabled, "missing setting preserves the published default")
	}
	require.Equal(t, 1, repo.getValueCalls, "request hot path uses the cache")
	other := &SettingService{settingRepo: repo}
	enabled, err := other.GetOpenAIRequestCompression(ctx)
	require.NoError(t, err)
	require.True(t, enabled)
	require.NoError(t, s.SetOpenAIRequestCompression(ctx, false))
	require.Equal(t, "false", repo.values[openAIRequestCompressionKey])
	enabled, err = s.GetOpenAIRequestCompression(ctx)
	require.NoError(t, err)
	require.False(t, enabled, "writer sees the saved switch immediately")
	other.openAIRequestCompressionCache.Store(&cachedOpenAIRequestCompression{enabled: true, until: time.Now().Add(-time.Second)})
	enabled, err = other.GetOpenAIRequestCompression(ctx)
	require.NoError(t, err)
	require.False(t, enabled, "another process reads the persisted switch after expiry")
	repo.setErr = errors.New("write failed")
	require.Error(t, s.SetOpenAIRequestCompression(ctx, true))
	enabled, err = s.GetOpenAIRequestCompression(ctx)
	require.NoError(t, err)
	require.False(t, enabled, "failed save must not replace the active cache")
	repo.setErr = nil
	require.NoError(t, s.SetOpenAIRequestCompression(ctx, true))
	enabled, err = s.GetOpenAIRequestCompression(ctx)
	require.NoError(t, err)
	require.True(t, enabled)
}

func TestOpenAIRequestCompressionSettingsInvalidOrUnavailable(t *testing.T) {
	for _, scenario := range []struct {
		name string
		raw  string
		err  error
	}{
		{"invalid", "TRUE", nil},
		{"database failure", "true", errors.New("read failed")},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			repo := &panelRateLimitSettingRepo{values: map[string]string{openAIRequestCompressionKey: scenario.raw}, getValueErr: scenario.err}
			s := &SettingService{settingRepo: repo}
			for range 2 {
				enabled, err := s.GetOpenAIRequestCompression(context.Background())
				require.Error(t, err)
				require.False(t, enabled)
			}
			require.Equal(t, 1, repo.getValueCalls, "briefly cache failures to avoid database storms")
		})
	}
}

func TestOpenAIRequestCompressionSettingsConcurrentLoad(t *testing.T) {
	repo := &panelRateLimitSettingRepo{}
	s := &SettingService{settingRepo: repo}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			enabled, err := s.GetOpenAIRequestCompression(context.Background())
			if err != nil || !enabled {
				t.Errorf("unexpected settings: enabled=%v, err=%v", enabled, err)
			}
		})
	}
	wg.Wait()
	require.Equal(t, 1, repo.getValueCalls)
}

func TestOpenAIOAuthRequestCompressionGlobalSwitch(t *testing.T) {
	repo := &requestCompressionSettingRepo{}
	settings := &SettingService{settingRepo: repo}
	upstream := &httpUpstreamRecorder{}
	s := &OpenAIGatewayService{httpUpstream: upstream, settingService: settings}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_request_compression": true}}
	forward := func(wantEncoding string) {
		t.Helper()
		upstream.resp = oauthCompressionResponse(200, "ok")
		resp, err := s.doOpenAIUpstream(oauthCompressionRequest(t, oauthCompressionBody()), "", account)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, wantEncoding, upstream.lastReq.Header.Get("Content-Encoding"))
	}
	forward("zstd")
	require.NoError(t, settings.SetOpenAIRequestCompression(context.Background(), false))
	forward("")
	require.NoError(t, settings.SetOpenAIRequestCompression(context.Background(), true))
	forward("zstd")
	account.Extra["openai_request_compression"] = false
	forward("")
	account.Extra["openai_request_compression"] = true
	repo.getValueErr = errors.New("database unavailable")
	settings.openAIRequestCompressionCache.Store(&cachedOpenAIRequestCompression{until: time.Now().Add(-time.Second)})
	forward("")
}
