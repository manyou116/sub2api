package service

import (
	"context"
	"errors"
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
