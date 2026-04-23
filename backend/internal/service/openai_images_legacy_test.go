package service

import (
	"testing"
	"time"
)

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

// TestSchedulerImageCapabilityFilter_LegacyImagesRateLimited 验证：
// 即使 image-native + OAuth + legacy 已启用，model_rate_limits[legacy_images] 在熔断窗口内
// 也应被调度跳过，但不影响 text 请求。
func TestSchedulerImageCapabilityFilter_LegacyImagesRateLimited(t *testing.T) {
scheduler := &defaultOpenAIAccountScheduler{}
groupOn := &Group{OpenAILegacyImagesDefault: true}
imageReq := OpenAIAccountScheduleRequest{RequiredImageCapability: OpenAIImagesCapabilityNative}
textReq := OpenAIAccountScheduleRequest{}

mkAccount := func(scopeReset string) *Account {
extra := map[string]any{
"openai_oauth_legacy_images": true,
}
if scopeReset != "" {
extra["model_rate_limits"] = map[string]any{
"legacy_images": map[string]any{
"rate_limited_at":     "2024-01-01T00:00:00Z",
"rate_limit_reset_at": scopeReset,
},
}
}
return &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: extra}
}

future := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)

cases := []struct {
name    string
account *Account
req     OpenAIAccountScheduleRequest
want    bool
}{
{"image_legacy_active_blocked", mkAccount(future), imageReq, false},
{"image_legacy_expired_passes", mkAccount(past), imageReq, true},
{"image_legacy_unset_passes", mkAccount(""), imageReq, true},
// text 请求完全不受 image-scope 限流影响
{"text_with_legacy_active_passes", mkAccount(future), textReq, true},
}
for _, tc := range cases {
t.Run(tc.name, func(t *testing.T) {
if got := scheduler.isAccountImageCapabilityCompatible(tc.account, tc.req, groupOn); got != tc.want {
t.Fatalf("isAccountImageCapabilityCompatible = %v, want %v", got, tc.want)
}
})
}
}

// TestLegacyImagesFailureCounter 验证 in-memory 计数器：
// 累加到阈值才返回 >= threshold；reset 后归零。
func TestLegacyImagesFailureCounter(t *testing.T) {
const aid int64 = 999999
legacyImagesResetFailure(aid)
defer legacyImagesResetFailure(aid)

if got := legacyImagesIncrementFailure(aid); got != 1 {
t.Fatalf("first increment = %d, want 1", got)
}
if got := legacyImagesIncrementFailure(aid); got != 2 {
t.Fatalf("second increment = %d, want 2", got)
}
if got := legacyImagesIncrementFailure(aid); got != 3 {
t.Fatalf("third increment = %d, want 3", got)
}
legacyImagesResetFailure(aid)
if got := legacyImagesIncrementFailure(aid); got != 1 {
t.Fatalf("post-reset increment = %d, want 1", got)
}
}

// TestIsLegacyOpenAIImageRateLimitStatus 验证显式限流识别：
// 429 / "rate limit" / "quota" / "you've reached" 命中。
func TestIsLegacyOpenAIImageRateLimitStatus(t *testing.T) {
cases := []struct {
name string
err  *legacyOpenAIImageStatusError
want bool
}{
{"nil_safe", nil, false},
{"http_429", &legacyOpenAIImageStatusError{StatusCode: 429}, true},
{"msg_rate_limit", &legacyOpenAIImageStatusError{StatusCode: 400, Message: "Rate limit reached"}, true},
{"body_quota", &legacyOpenAIImageStatusError{StatusCode: 400, ResponseBody: []byte(`{"detail":"quota exceeded"}`)}, true},
{"msg_youve_reached", &legacyOpenAIImageStatusError{StatusCode: 200, Message: "You've reached your daily limit"}, true},
{"unrelated_400", &legacyOpenAIImageStatusError{StatusCode: 400, Message: "bad request"}, false},
{"unrelated_500", &legacyOpenAIImageStatusError{StatusCode: 500, Message: "internal server error"}, false},
}
for _, tc := range cases {
t.Run(tc.name, func(t *testing.T) {
if got := isLegacyOpenAIImageRateLimitStatus(tc.err); got != tc.want {
t.Fatalf("isLegacyOpenAIImageRateLimitStatus = %v, want %v", got, tc.want)
}
})
}
}
