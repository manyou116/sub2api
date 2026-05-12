// Package service - Kiro token refresh
//
// 提供 Kiro 平台两种认证方式（Social / IdC）的 access_token 刷新：
//
//   - Social：直接 POST https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken
//     仅需 refreshToken。
//
//   - IdC：走 AWS SSO OIDC create-token 接口
//     POST https://oidc.{region}.amazonaws.com/token
//     form: grant_type=refresh_token, client_id, client_secret, refresh_token
//
// 错误语义：
//
//	401 -> ErrKiroAuthFailed     (refresh token 无效，需要重新导入)
//	423 / 403+TemporarilySuspended -> ErrKiroBanned (账号被封禁)
//	429 -> ErrKiroRateLimited    (上游限流，调用方应屏蔽该账号)
//	其他 -> wrapped error
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
)

// ============== 公共错误 / 模型 ==============

// ErrKiroAuthFailed 表示账号 token 已失效（refresh token 不可用、需要重新导入）。
var ErrKiroAuthFailed = errors.New("kiro: auth failed")

// ErrKiroBanned 表示账号已被 Kiro 服务端封禁。
var ErrKiroBanned = errors.New("kiro: account suspended")

// ErrKiroRateLimited 表示上游 429 限流。
var ErrKiroRateLimited = errors.New("kiro: rate limited")

// KiroTokenInfo 是 RefreshKiroToken 返回的统一结构。
// camelCase 字段直接来自 Kiro 上游响应，便于回写 Account.Credentials。
type KiroTokenInfo struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	IDToken      string `json:"idToken,omitempty"`
	ExpiresIn    int64  `json:"expiresIn"`           // 秒
	ExpiresAt    int64  `json:"expiresAt,omitempty"` // unix
	ProfileArn   string `json:"profileArn,omitempty"`

	// IdC 流程上游有时会续发新的 client 凭证；按需保留
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

// ============== HTTP 端点常量 ==============

const (
	kiroDesktopAuthBase   = "https://prod.us-east-1.auth.desktop.kiro.dev"
	kiroSocialRefreshPath = "/refreshToken"
	awsSSOIdCTokenPathTpl = "https://oidc.%s.amazonaws.com/token"

	kiroHTTPTimeout = 30 * time.Second
)

// ============== Service ==============

// KiroTokenService 负责刷新 Kiro 平台账号 token。
// 无状态，可全局复用。proxyURL 通过参数传入（账号绑定的 proxy）。
type KiroTokenService struct{}

// NewKiroTokenService 构造一个 KiroTokenService。
func NewKiroTokenService() *KiroTokenService {
	return &KiroTokenService{}
}

// RefreshAccountToken 根据账号上的 auth_method 自动派发到 Social / IdC 两种刷新流程。
// 成功后返回新 token，调用方负责把字段回写到 Account.Credentials 并持久化。
//
// 不在内部直接落库，是为了让上层（账号服务/调度器）控制并发、写入策略。
func (s *KiroTokenService) RefreshAccountToken(ctx context.Context, account *Account, proxyURL string) (*KiroTokenInfo, error) {
	if account == nil || !account.IsKiro() {
		return nil, fmt.Errorf("kiro: account is nil or not a Kiro account")
	}
	refreshToken := account.KiroRefreshToken()
	if refreshToken == "" {
		return nil, fmt.Errorf("kiro: account has no refresh_token")
	}
	machineID := account.KiroMachineID()
	if machineID == "" {
		return nil, fmt.Errorf("kiro: account has no machine_id")
	}

	switch account.KiroAuthMethod() {
	case KiroAuthMethodIdC:
		clientID := account.KiroClientID()
		clientSecret := account.KiroClientSecret()
		if clientID == "" || clientSecret == "" {
			return nil, fmt.Errorf("kiro: IdC account missing client_id/client_secret")
		}
		return s.refreshIdC(ctx, refreshIdCParams{
			Region:       account.KiroRegion(),
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RefreshToken: refreshToken,
			MachineID:    machineID,
			ProxyURL:     proxyURL,
		})
	case KiroAuthMethodSocial:
		fallthrough
	default:
		return s.refreshSocial(ctx, refreshSocialParams{
			RefreshToken: refreshToken,
			MachineID:    machineID,
			ProxyURL:     proxyURL,
		})
	}
}

// ============== Social refresh ==============

type refreshSocialParams struct {
	RefreshToken string
	MachineID    string
	ProxyURL     string
}

type kiroSocialRefreshReq struct {
	RefreshToken string `json:"refreshToken"`
}

// kiroSocialRefreshResp 兼容上游可能返回的两种字段命名风格：
//   - camelCase（Kiro IDE 桌面端常见）：accessToken / refreshToken / expiresIn / idToken / profileArn
//   - snake_case（GitHub OAuth/OIDC 标准）：access_token / refresh_token / expires_in / id_token / profile_arn
//
// json.Unmarshal 对每个目标字段尝试匹配 tag，多 tag 同 struct 不支持，因此用并列字段后合并。
type kiroSocialRefreshResp struct {
	AccessToken       string `json:"accessToken"`
	AccessTokenSnake  string `json:"access_token"`
	RefreshToken      string `json:"refreshToken"`
	RefreshTokenSnake string `json:"refresh_token"`
	ExpiresIn         int64  `json:"expiresIn"`
	ExpiresInSnake    int64  `json:"expires_in"`
	IDToken           string `json:"idToken,omitempty"`
	IDTokenSnake      string `json:"id_token,omitempty"`
	ProfileArn        string `json:"profileArn,omitempty"`
	ProfileArnSnake   string `json:"profile_arn,omitempty"`
}

func (r *kiroSocialRefreshResp) normalize() {
	if r.AccessToken == "" {
		r.AccessToken = r.AccessTokenSnake
	}
	if r.RefreshToken == "" {
		r.RefreshToken = r.RefreshTokenSnake
	}
	if r.ExpiresIn == 0 {
		r.ExpiresIn = r.ExpiresInSnake
	}
	if r.IDToken == "" {
		r.IDToken = r.IDTokenSnake
	}
	if r.ProfileArn == "" {
		r.ProfileArn = r.ProfileArnSnake
	}
}

func (s *KiroTokenService) refreshSocial(ctx context.Context, p refreshSocialParams) (*KiroTokenInfo, error) {
	body, _ := json.Marshal(kiroSocialRefreshReq{RefreshToken: p.RefreshToken})

	endpoint := kiroDesktopAuthBase + kiroSocialRefreshPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("kiro social: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf(KiroIDEUserAgentTmpl, p.MachineID))

	respBody, status, err := doKiroHTTP(ctx, req, p.ProxyURL)
	if err != nil {
		return nil, err
	}
	if mapped := mapKiroStatusErr(status, respBody); mapped != nil {
		return nil, mapped
	}

	var data kiroSocialRefreshResp
	if err := json.Unmarshal(respBody, &data); err != nil {
		return nil, fmt.Errorf("kiro social: parse response: %w (body=%s)", err, truncateBody(respBody))
	}
	data.normalize()
	if data.AccessToken == "" {
		return nil, fmt.Errorf("kiro social: empty access_token in response (body=%s)", truncateBody(respBody))
	}

	// Social 刷新有些场景上游不返回新的 refreshToken，沿用旧值
	rt := data.RefreshToken
	rotated := rt != "" && rt != p.RefreshToken
	if rt == "" {
		rt = p.RefreshToken
	}

	// 诊断日志：定位 "Bad credentials" 反复需要重新导入 RT 的根因。
	// 只打印响应字段名集合 + 是否 rotation，**不打印 token 明文**。
	slog.Info("kiro_social_refresh_ok",
		"upstream_keys", upstreamJSONKeys(respBody),
		"rt_rotated", rotated,
		"rt_returned_empty", data.RefreshToken == "",
		"expires_in", data.ExpiresIn,
		"machine_id_set", p.MachineID != "",
	)

	return &KiroTokenInfo{
		AccessToken:  data.AccessToken,
		RefreshToken: rt,
		IDToken:      data.IDToken,
		ExpiresIn:    data.ExpiresIn,
		ExpiresAt:    time.Now().Unix() + data.ExpiresIn,
		ProfileArn:   data.ProfileArn,
	}, nil
}

// ============== IdC refresh (AWS SSO OIDC) ==============

type refreshIdCParams struct {
	Region       string
	ClientID     string
	ClientSecret string
	RefreshToken string
	MachineID    string
	ProxyURL     string
}

// AWS SSO OIDC create-token JSON 响应（grant_type=refresh_token）
// https://docs.aws.amazon.com/singlesignon/latest/OIDCAPIReference/API_CreateToken.html
type ssoOIDCCreateTokenReq struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	GrantType    string `json:"grantType"`
	RefreshToken string `json:"refreshToken"`
}

type ssoOIDCCreateTokenResp struct {
	AccessToken  string `json:"accessToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int64  `json:"expiresIn"`
	RefreshToken string `json:"refreshToken"`
	IDToken      string `json:"idToken"`
}

func (s *KiroTokenService) refreshIdC(ctx context.Context, p refreshIdCParams) (*KiroTokenInfo, error) {
	body, _ := json.Marshal(ssoOIDCCreateTokenReq{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		GrantType:    "refresh_token",
		RefreshToken: p.RefreshToken,
	})

	region := strings.TrimSpace(p.Region)
	if region == "" {
		region = KiroDefaultRegion
	}
	endpoint := fmt.Sprintf(awsSSOIdCTokenPathTpl, region)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("kiro idc: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf(KiroIDEUserAgentTmpl, p.MachineID))

	respBody, status, err := doKiroHTTP(ctx, req, p.ProxyURL)
	if err != nil {
		return nil, err
	}
	if mapped := mapKiroStatusErr(status, respBody); mapped != nil {
		return nil, mapped
	}

	var data ssoOIDCCreateTokenResp
	if err := json.Unmarshal(respBody, &data); err != nil {
		return nil, fmt.Errorf("kiro idc: parse response: %w (body=%s)", err, truncateBody(respBody))
	}
	if data.AccessToken == "" {
		return nil, fmt.Errorf("kiro idc: empty access_token in response")
	}

	rt := data.RefreshToken
	if rt == "" {
		rt = p.RefreshToken
	}

	return &KiroTokenInfo{
		AccessToken:  data.AccessToken,
		RefreshToken: rt,
		IDToken:      data.IDToken,
		ExpiresIn:    data.ExpiresIn,
		ExpiresAt:    time.Now().Unix() + data.ExpiresIn,
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
	}, nil
}

// ============== 工具函数 ==============

// doKiroHTTP 发送请求，统一处理超时/代理/响应读取。
func doKiroHTTP(_ context.Context, req *http.Request, proxyURL string) ([]byte, int, error) {
	client, err := httpclient.GetClient(httpclient.Options{
		ProxyURL:           strings.TrimSpace(proxyURL),
		Timeout:            kiroHTTPTimeout,
		ValidateResolvedIP: true,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("kiro: build http client: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("kiro: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB 兜底
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("kiro: read response: %w", err)
	}
	return body, resp.StatusCode, nil
}

// mapKiroStatusErr 把 HTTP 状态码 + body 映射为 sentinel 错误。
// 返回 nil 表示状态码 OK，调用方继续解析 body。
func mapKiroStatusErr(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	bodyStr := string(body)
	switch status {
	case http.StatusUnauthorized: // 401
		return fmt.Errorf("%w: %s", ErrKiroAuthFailed, truncateBody(body))
	case http.StatusLocked: // 423
		return fmt.Errorf("%w: %s", ErrKiroBanned, truncateBody(body))
	case http.StatusForbidden: // 403
		// 仅当响应里出现 TemporarilySuspended 时才认定为封禁；
		// 其他 403 当作 auth 失败。
		if strings.Contains(bodyStr, "TemporarilySuspended") || strings.Contains(bodyStr, "Suspended") {
			return fmt.Errorf("%w: %s", ErrKiroBanned, truncateBody(body))
		}
		return fmt.Errorf("%w: %s", ErrKiroAuthFailed, truncateBody(body))
	case http.StatusTooManyRequests: // 429
		return fmt.Errorf("%w: %s", ErrKiroRateLimited, truncateBody(body))
	default:
		return fmt.Errorf("kiro: HTTP %d: %s", status, truncateBody(body))
	}
}

// truncateBody 保护日志不被巨大响应淹没。
func truncateBody(b []byte) string {
	const maxLen = 512
	if len(b) <= maxLen {
		return string(b)
	}
	return string(b[:maxLen]) + "...[truncated]"
}

// upstreamJSONKeys 把响应体顶层 JSON object 的字段名提取为有序列表，
// 便于诊断字段名命名风格（camelCase vs snake_case）问题，不泄漏 token 明文。
func upstreamJSONKeys(body []byte) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	return keys
}

// ApplyKiroTokenInfo 把刷新成功的 token 写回 account.Credentials，
// 同步 ExpiresAt 字段。返回新的 credentials map 副本，调用方负责 persist。
//
// 用法：
//
//	creds := ApplyKiroTokenInfo(account, info)
//	account.Credentials = creds
//	account.ExpiresAt = ...
//	repo.Update(account)
func ApplyKiroTokenInfo(account *Account, info *KiroTokenInfo) map[string]any {
	if account == nil || info == nil {
		return nil
	}
	creds := make(map[string]any, len(account.Credentials)+5)
	for k, v := range account.Credentials {
		creds[k] = v
	}
	creds["access_token"] = info.AccessToken
	if info.RefreshToken != "" {
		creds["refresh_token"] = info.RefreshToken
	}
	if info.IDToken != "" {
		creds["id_token"] = info.IDToken
	}
	if info.ProfileArn != "" {
		creds["profile_arn"] = info.ProfileArn
	}
	if info.ClientID != "" {
		creds["client_id"] = info.ClientID
	}
	if info.ClientSecret != "" {
		creds["client_secret"] = info.ClientSecret
	}
	if info.ExpiresAt > 0 {
		creds["expires_at"] = info.ExpiresAt
	}
	return creds
}

// keep url package used (for future PKCE state encoding when login flow added)
var _ = url.QueryEscape
