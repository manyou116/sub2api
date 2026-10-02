package service

import (
	"strings"
)

// IsKiro 返回该账号是否为 Kiro 平台账号。
func (a *Account) IsKiro() bool {
	return a.Platform == PlatformKiro
}

// KiroAuthMethod 返回 Kiro 账号的认证方式（"social" / "idc"）。
// 默认为 social。
func (a *Account) KiroAuthMethod() string {
	if !a.IsKiro() {
		return ""
	}
	v := strings.ToLower(strings.TrimSpace(a.GetCredential("auth_method")))
	switch v {
	case KiroAuthMethodIdC, "i_dc", "iam_identity_center":
		return KiroAuthMethodIdC
	case KiroAuthMethodSocial, "":
		return KiroAuthMethodSocial
	}
	return KiroAuthMethodSocial
}

// KiroProvider 返回 Kiro 账号的身份提供方（Google/Github/BuilderId/Enterprise）。
func (a *Account) KiroProvider() string {
	return a.GetCredential("provider")
}

// KiroEmail 返回账号绑定邮箱（仅供展示）。
func (a *Account) KiroEmail() string {
	return a.GetCredential("email")
}

// KiroRefreshToken 返回 refresh_token。
func (a *Account) KiroRefreshToken() string {
	if !a.IsKiro() {
		return ""
	}
	return a.GetCredential("refresh_token")
}

// KiroAccessToken 返回当前缓存的 access_token。
func (a *Account) KiroAccessToken() string {
	if !a.IsKiro() {
		return ""
	}
	return a.GetCredential("access_token")
}

// KiroProfileArn 返回 Social 流程的 profileArn（CodeWhisperer 调用必需）。
func (a *Account) KiroProfileArn() string {
	return a.GetCredential("profile_arn")
}

// KiroMachineID 返回与 Token 绑定的 machineId，必填。
func (a *Account) KiroMachineID() string {
	return a.GetCredential("machine_id")
}

// KiroClientID 返回 IdC 流程刷新 token 必需的 client_id。
func (a *Account) KiroClientID() string {
	return a.GetCredential("client_id")
}

// KiroClientSecret 返回 IdC 流程的 client_secret（JWT）。
func (a *Account) KiroClientSecret() string {
	return a.GetCredential("client_secret")
}

// KiroRegion 返回 IdC AWS SSO OIDC 所在 region，默认 us-east-1。
func (a *Account) KiroRegion() string {
	r := strings.TrimSpace(a.GetCredential("region"))
	if r == "" {
		return KiroDefaultRegion
	}
	return r
}

// KiroUsageData 返回最近一次配额探测保存在 Extra["kiro_usage_data"] 的原始结构。
// 不存在或类型不匹配时返回 nil。
func (a *Account) KiroUsageData() map[string]any {
	if a == nil || a.Extra == nil {
		return nil
	}
	v, ok := a.Extra["kiro_usage_data"]
	if !ok {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// KiroRemainingQuota 返回剩余配额（usageLimit - currentUsage）。
//   - 配额信息缺失 → 返回 +Inf（不影响调度，按概率打散）
//   - overageEnabled=true 且已超额 → 返回 0.0001（最低优先级但仍可用）
func (a *Account) KiroRemainingQuota() float64 {
	cur, lim, overage := a.kiroUsageBreakdown()
	if lim <= 0 {
		return 1e9
	}
	rem := lim - cur
	if rem <= 0 {
		if overage {
			return 0.0001
		}
		return 0
	}
	return rem
}

func (a *Account) kiroUsageBreakdown() (cur, lim float64, overageEnabled bool) {
	usage := a.KiroUsageData()
	if usage == nil {
		return 0, 0, false
	}
	list, ok := usage["usageBreakdownList"].([]any)
	if !ok || len(list) == 0 {
		return 0, 0, false
	}
	first, ok := list[0].(map[string]any)
	if !ok {
		return 0, 0, false
	}
	cur = numAsFloat(first["currentUsage"])
	lim = numAsFloat(first["usageLimit"])
	if oc, ok := first["overageConfiguration"].(map[string]any); ok {
		if v, ok := oc["overageEnabled"].(bool); ok {
			overageEnabled = v
		}
	}
	return
}

// numAsFloat 把 JSON 解析得到的数字（float64/int/int64/json.Number）转 float。
func numAsFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	}
	return 0
}

// KiroProxyURL returns the bound proxy URL for upstream Kiro calls, or "".
func (a *Account) KiroProxyURL() string {
	if a == nil || a.Proxy == nil {
		return ""
	}
	return a.Proxy.URL()
}
