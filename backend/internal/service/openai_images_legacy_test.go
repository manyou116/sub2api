package service

import "testing"

// TestIsOpenAILegacyImagesEnabled_ThreeState 验证三态判定矩阵：
// account explicit > group default > false。
func TestIsOpenAILegacyImagesEnabled_ThreeState(t *testing.T) {
	oauthAccount := func(extra map[string]any) *Account {
		return &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: extra}
	}
	apiKeyAccount := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"openai_oauth_legacy_images": true}}
	groupOn := &Group{OpenAILegacyImagesDefault: true}
	groupOff := &Group{OpenAILegacyImagesDefault: false}

	cases := []struct {
		name    string
		account *Account
		group   *Group
		want    bool
	}{
		{"account_true_overrides_group_off", oauthAccount(map[string]any{"openai_oauth_legacy_images": true}), groupOff, true},
		{"account_false_overrides_group_on", oauthAccount(map[string]any{"openai_oauth_legacy_images": false}), groupOn, false},
		{"account_unset_falls_back_to_group_on", oauthAccount(nil), groupOn, true},
		{"account_unset_falls_back_to_group_off", oauthAccount(nil), groupOff, false},
		{"account_unset_no_group", oauthAccount(nil), nil, false},
		{"account_extra_empty_map_no_group", oauthAccount(map[string]any{}), nil, false},
		{"non_oauth_account_ignored_even_if_group_on", apiKeyAccount, groupOn, false},
		{"nil_account_safe", nil, groupOn, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.account.IsOpenAILegacyImagesEnabled(tc.group); got != tc.want {
				t.Fatalf("IsOpenAILegacyImagesEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}
