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

// TestSchedulerImageCapabilityFilter 验证 image-native 调度过滤：
// 仅在 RequiredImageCapability=native + OAuth 账号 上检查 IsOpenAILegacyImagesEnabled；
// text 请求 / APIKey 账号 一律放行（不影响其他模型调度）。
func TestSchedulerImageCapabilityFilter(t *testing.T) {
scheduler := &defaultOpenAIAccountScheduler{}
oauthLegacyOn := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_oauth_legacy_images": true}}
oauthLegacyOff := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_oauth_legacy_images": false}}
oauthDefault := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
apiKeyAcct := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

groupOn := &Group{OpenAILegacyImagesDefault: true}
groupOff := &Group{OpenAILegacyImagesDefault: false}

imageReq := OpenAIAccountScheduleRequest{RequiredImageCapability: OpenAIImagesCapabilityNative}
textReq := OpenAIAccountScheduleRequest{}

cases := []struct {
name    string
account *Account
req     OpenAIAccountScheduleRequest
group   *Group
want    bool
}{
{"text_request_oauth_legacy_off_passes", oauthLegacyOff, textReq, groupOff, true},
{"text_request_oauth_default_passes", oauthDefault, textReq, groupOff, true},
{"image_apikey_passes", apiKeyAcct, imageReq, groupOff, true},
{"image_oauth_legacy_on_passes", oauthLegacyOn, imageReq, groupOff, true},
{"image_oauth_legacy_off_blocked", oauthLegacyOff, imageReq, groupOn, false},
{"image_oauth_default_with_group_on_passes", oauthDefault, imageReq, groupOn, true},
{"image_oauth_default_with_group_off_blocked", oauthDefault, imageReq, groupOff, false},
{"image_oauth_default_no_group_blocked", oauthDefault, imageReq, nil, false},
{"nil_account_blocked", nil, imageReq, groupOn, false},
}
for _, tc := range cases {
t.Run(tc.name, func(t *testing.T) {
if got := scheduler.isAccountImageCapabilityCompatible(tc.account, tc.req, tc.group); got != tc.want {
t.Fatalf("isAccountImageCapabilityCompatible = %v, want %v", got, tc.want)
}
})
}
}
