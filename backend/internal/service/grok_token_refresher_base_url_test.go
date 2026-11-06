//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

type grokBaseURLRefreshOAuthStub struct {
	GrokOAuthTokenService
}

func (*grokBaseURLRefreshOAuthStub) RefreshAccountToken(context.Context, *Account) (*GrokTokenInfo, error) {
	return &GrokTokenInfo{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: 1_900_000_000}, nil
}

func (*grokBaseURLRefreshOAuthStub) BuildAccountCredentials(info *GrokTokenInfo) map[string]any {
	return (&GrokOAuthService{}).BuildAccountCredentials(info)
}

func TestGrokTokenRefresherPreservesGlobalUpstreamAfterRefresh(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL any
		want    string
	}{
		{name: "unset", want: ""},
		{name: "empty", baseURL: "", want: ""},
		{name: "whitespace", baseURL: "  ", want: ""},
		{name: "pinned CLI", baseURL: xai.DefaultCLIBaseURL, want: xai.DefaultCLIBaseURL},
		{name: "pinned regional", baseURL: xai.DefaultEUWest1BaseURL, want: xai.DefaultEUWest1BaseURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credentials := map[string]any{"refresh_token": "old-refresh", "custom_setting": "preserved"}
			if tc.baseURL != nil {
				credentials["base_url"] = tc.baseURL
			}
			account := &Account{Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: credentials}
			refresher := NewGrokTokenRefresher(&grokBaseURLRefreshOAuthStub{})
			updated, err := refresher.Refresh(context.Background(), account)
			require.NoError(t, err)
			require.Equal(t, tc.want, updated["base_url"])
			require.Equal(t, "new-access", updated["access_token"])
			require.Equal(t, "new-refresh", updated["refresh_token"])
			require.Equal(t, "preserved", updated["custom_setting"])
			account.Credentials = updated
			settings := NewSettingService(&grokBaseURLSettingRepoStub{values: map[string]string{
				SettingKeyGrokDefaultBaseURLMode: GrokDefaultBaseURLModeUSWest2,
			}}, nil)
			want := tc.want
			if want == "" {
				want = xai.DefaultUSWest2BaseURL
			}
			require.Equal(t, want, settings.ResolveGrokBaseURL(context.Background(), account))
		})
	}
}
