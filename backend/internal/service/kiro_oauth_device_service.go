package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// KiroOAuthDeviceService 实现 AWS IdC Device Code Flow，
// 用于在 Web 后台让用户通过 Builder ID / 企业 IdC 登录新的 Kiro 账号。
//
// 流程参考 aiclient2api kiro-oauth.js：
//  1. POST /client/register        → clientId, clientSecret
//  2. POST /device_authorization   → deviceCode, userCode, verificationUriComplete
//  3. POST /token (grant_type=device_code, 轮询)  → access/refresh token
//
// 端点：https://oidc.{region}.amazonaws.com/...
// User-Agent 必须为 KiroIDE
type KiroOAuthDeviceService struct {
	tokenSvc *KiroTokenService

	mu       sync.RWMutex
	sessions map[string]*kiroOAuthSession // sessionId -> state
}

// NewKiroOAuthDeviceService 构造服务（无外部依赖，可单例）。
func NewKiroOAuthDeviceService(tokenSvc *KiroTokenService) *KiroOAuthDeviceService {
	if tokenSvc == nil {
		tokenSvc = NewKiroTokenService()
	}
	s := &KiroOAuthDeviceService{
		tokenSvc: tokenSvc,
		sessions: make(map[string]*kiroOAuthSession),
	}
	go s.gcLoop()
	return s
}

// ============== 默认配置 ==============

const (
	KiroOAuthDefaultStartUrl = "https://view.awsapps.com/start"
	KiroOAuthDefaultRegion   = "us-east-1"

	kiroOAuthClientName = "Kiro IDE"
	kiroOAuthClientType = "public"

	kiroOAuthSessionTTL = 15 * time.Minute
	kiroOAuthMinPoll    = 3 * time.Second
)

var kiroOAuthScopes = []string{
	"codewhisperer:completions",
	"codewhisperer:analysis",
	"codewhisperer:conversations",
}

// ============== 公开 DTO ==============

// StartDeviceAuthInput 开启一次 Device Code 登录会话。
type StartDeviceAuthInput struct {
	StartUrl string // 默认 https://view.awsapps.com/start
	Region   string // 默认 us-east-1
	ProxyURL string // 透传给 HTTP client
}

// StartDeviceAuthResult 返给前端展示的字段。
type StartDeviceAuthResult struct {
	SessionID               string `json:"sessionId"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	UserCode                string `json:"userCode"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

// PollResult 是 GET /poll 的响应；
// status: pending | done | error
type PollResult struct {
	Status     string `json:"status"`
	AccountID  int64  `json:"accountId,omitempty"`
	Email      string `json:"email,omitempty"`
	Error      string `json:"error,omitempty"`
	UserCode   string `json:"userCode,omitempty"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
}

// ============== 内部状态 ==============

type kiroOAuthSession struct {
	mu sync.Mutex

	SessionID               string
	Region                  string
	StartUrl                string
	ClientID                string
	ClientSecret            string
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                int
	ExpiresAt               time.Time
	StartedAt               time.Time

	Status     string // pending | done | error
	Error      string
	AccountID  int64
	Email      string
	FinishedAt time.Time
}

// ============== 1. RegisterClient ==============

type ssoRegisterClientReq struct {
	ClientName string   `json:"clientName"`
	ClientType string   `json:"clientType"`
	Scopes     []string `json:"scopes"`
}

type ssoRegisterClientResp struct {
	ClientID              string `json:"clientId"`
	ClientSecret          string `json:"clientSecret"`
	ClientIDIssuedAt      int64  `json:"clientIdIssuedAt"`
	ClientSecretExpiresAt int64  `json:"clientSecretExpiresAt"`
	AuthorizationEndpoint string `json:"authorizationEndpoint,omitempty"`
	TokenEndpoint         string `json:"tokenEndpoint,omitempty"`
}

func (s *KiroOAuthDeviceService) registerClient(ctx context.Context, region, proxyURL string) (string, string, error) {
	body, _ := json.Marshal(ssoRegisterClientReq{
		ClientName: kiroOAuthClientName,
		ClientType: kiroOAuthClientType,
		Scopes:     kiroOAuthScopes,
	})
	endpoint := fmt.Sprintf("https://oidc.%s.amazonaws.com/client/register", region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("kiro oauth register: build req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "KiroIDE")

	respBody, status, err := doKiroHTTP(ctx, req, proxyURL)
	if err != nil {
		return "", "", err
	}
	if mapped := mapKiroStatusErr(status, respBody); mapped != nil {
		return "", "", mapped
	}
	var data ssoRegisterClientResp
	if err := json.Unmarshal(respBody, &data); err != nil {
		return "", "", fmt.Errorf("kiro oauth register: parse: %w (body=%s)", err, truncateBody(respBody))
	}
	if data.ClientID == "" || data.ClientSecret == "" {
		return "", "", fmt.Errorf("kiro oauth register: empty clientId/clientSecret")
	}
	return data.ClientID, data.ClientSecret, nil
}

// ============== 2. StartDeviceAuth ==============

type ssoDeviceAuthReq struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	StartURL     string `json:"startUrl"`
}

type ssoDeviceAuthResp struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

// OAuthSuccessContext 在 OAuth 成功回调中提供必要的会话元数据，
// 让上层 handler 知道账号是 IdC 还是企业 / 用什么 region 等。
type OAuthSuccessContext struct {
	SessionID    string
	Region       string
	StartUrl     string
	ClientID     string
	ClientSecret string
}

// StartDeviceAuth 完成 register + device_authorization，
// 把 session 写入内存并启动后台轮询。
func (s *KiroOAuthDeviceService) StartDeviceAuth(
	ctx context.Context,
	in StartDeviceAuthInput,
	onSuccess func(ctx context.Context, info *KiroTokenInfo, ctxMeta OAuthSuccessContext) (int64, string, error),
) (*StartDeviceAuthResult, error) {
	region := strings.TrimSpace(in.Region)
	if region == "" {
		region = KiroOAuthDefaultRegion
	}
	startUrl := strings.TrimSpace(in.StartUrl)
	if startUrl == "" {
		startUrl = KiroOAuthDefaultStartUrl
	}

	clientID, clientSecret, err := s.registerClient(ctx, region, in.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("register client: %w", err)
	}

	body, _ := json.Marshal(ssoDeviceAuthReq{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		StartURL:     startUrl,
	})
	endpoint := fmt.Sprintf("https://oidc.%s.amazonaws.com/device_authorization", region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("kiro oauth device_authorization: build req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "KiroIDE")

	respBody, status, err := doKiroHTTP(ctx, req, in.ProxyURL)
	if err != nil {
		return nil, err
	}
	if mapped := mapKiroStatusErr(status, respBody); mapped != nil {
		return nil, mapped
	}

	var data ssoDeviceAuthResp
	if err := json.Unmarshal(respBody, &data); err != nil {
		return nil, fmt.Errorf("kiro oauth device_authorization: parse: %w (body=%s)", err, truncateBody(respBody))
	}
	if data.DeviceCode == "" || data.UserCode == "" {
		return nil, fmt.Errorf("kiro oauth device_authorization: empty deviceCode/userCode")
	}

	interval := data.Interval
	if interval <= 0 {
		interval = 5
	}
	expiresIn := data.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}

	sess := &kiroOAuthSession{
		SessionID:               uuid.NewString(),
		Region:                  region,
		StartUrl:                startUrl,
		ClientID:                clientID,
		ClientSecret:            clientSecret,
		DeviceCode:              data.DeviceCode,
		UserCode:                data.UserCode,
		VerificationURI:         data.VerificationURI,
		VerificationURIComplete: data.VerificationURIComplete,
		Interval:                interval,
		ExpiresAt:               time.Now().Add(time.Duration(expiresIn) * time.Second),
		StartedAt:               time.Now(),
		Status:                  "pending",
	}
	s.mu.Lock()
	s.sessions[sess.SessionID] = sess
	s.mu.Unlock()

	go s.runPolling(sess, in.ProxyURL, onSuccess)

	return &StartDeviceAuthResult{
		SessionID:               sess.SessionID,
		VerificationURI:         data.VerificationURI,
		VerificationURIComplete: data.VerificationURIComplete,
		UserCode:                data.UserCode,
		ExpiresIn:               expiresIn,
		Interval:                interval,
	}, nil
}

// ============== 3. PollToken ==============

type ssoCreateTokenReq struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	GrantType    string `json:"grantType"`
	DeviceCode   string `json:"deviceCode"`
}

type ssoCreateTokenResp struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	IDToken      string `json:"idToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	TokenType    string `json:"tokenType"`
}

// pollOnce 单次 token 调用；
//   - (info, nil) 成功
//   - (nil, "pending") 仍在等待用户授权
//   - (nil, fatalErr) 致命错误（Expired/AccessDenied/...）
func (s *KiroOAuthDeviceService) pollOnce(ctx context.Context, sess *kiroOAuthSession, proxyURL string) (*KiroTokenInfo, string, error) {
	body, _ := json.Marshal(ssoCreateTokenReq{
		ClientID:     sess.ClientID,
		ClientSecret: sess.ClientSecret,
		GrantType:    "urn:ietf:params:oauth:grant-type:device_code",
		DeviceCode:   sess.DeviceCode,
	})
	endpoint := fmt.Sprintf("https://oidc.%s.amazonaws.com/token", sess.Region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("build req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "KiroIDE")

	respBody, status, err := doKiroHTTP(ctx, req, proxyURL)
	if err != nil {
		return nil, "", err
	}

	if status >= 200 && status < 300 {
		var data ssoCreateTokenResp
		if err := json.Unmarshal(respBody, &data); err != nil {
			return nil, "", fmt.Errorf("parse token resp: %w (body=%s)", err, truncateBody(respBody))
		}
		if data.AccessToken == "" {
			return nil, "", fmt.Errorf("empty accessToken in token resp")
		}
		return &KiroTokenInfo{
			AccessToken:  data.AccessToken,
			RefreshToken: data.RefreshToken,
			IDToken:      data.IDToken,
			ExpiresIn:    data.ExpiresIn,
			ExpiresAt:    time.Now().Unix() + data.ExpiresIn,
			ClientID:     sess.ClientID,
			ClientSecret: sess.ClientSecret,
		}, "", nil
	}

	bodyStr := string(respBody)
	// AuthorizationPendingException: 用户尚未点同意；继续轮询
	if strings.Contains(bodyStr, "AuthorizationPending") {
		return nil, "pending", nil
	}
	// SlowDownException: 加速太快，下次延长 interval
	if strings.Contains(bodyStr, "SlowDown") {
		return nil, "slow_down", nil
	}
	return nil, "", fmt.Errorf("HTTP %d: %s", status, truncateBody(respBody))
}

// runPolling 后台轮询直到成功 / 失败 / session 过期。
func (s *KiroOAuthDeviceService) runPolling(
	sess *kiroOAuthSession,
	proxyURL string,
	onSuccess func(ctx context.Context, info *KiroTokenInfo, ctxMeta OAuthSuccessContext) (int64, string, error),
) {
	interval := time.Duration(sess.Interval) * time.Second
	if interval < kiroOAuthMinPoll {
		interval = kiroOAuthMinPoll
	}

	for {
		// 过期检查
		if time.Now().After(sess.ExpiresAt) {
			s.markError(sess, "device code expired")
			return
		}
		time.Sleep(interval)

		ctx, cancel := context.WithTimeout(context.Background(), kiroHTTPTimeout)
		info, state, err := s.pollOnce(ctx, sess, proxyURL)
		cancel()

		if state == "pending" {
			continue
		}
		if state == "slow_down" {
			interval += 2 * time.Second
			continue
		}
		if err != nil {
			s.markError(sess, err.Error())
			return
		}

		// 成功，落库
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		accountID, email, createErr := onSuccess(ctx2, info, OAuthSuccessContext{
			SessionID:    sess.SessionID,
			Region:       sess.Region,
			StartUrl:     sess.StartUrl,
			ClientID:     sess.ClientID,
			ClientSecret: sess.ClientSecret,
		})
		cancel2()
		if createErr != nil {
			s.markError(sess, "create account: "+createErr.Error())
			return
		}
		s.markDone(sess, accountID, email)
		return
	}
}

func (s *KiroOAuthDeviceService) markDone(sess *kiroOAuthSession, accountID int64, email string) {
	sess.mu.Lock()
	sess.Status = "done"
	sess.AccountID = accountID
	sess.Email = email
	sess.FinishedAt = time.Now()
	sess.mu.Unlock()
}

func (s *KiroOAuthDeviceService) markError(sess *kiroOAuthSession, msg string) {
	sess.mu.Lock()
	sess.Status = "error"
	sess.Error = msg
	sess.FinishedAt = time.Now()
	sess.mu.Unlock()
}

// Poll 查询 session 状态（HTTP handler 用）。
func (s *KiroOAuthDeviceService) Poll(sessionID string) (*PollResult, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	r := &PollResult{
		Status:    sess.Status,
		AccountID: sess.AccountID,
		Email:     sess.Email,
		Error:     sess.Error,
		UserCode:  sess.UserCode,
		StartedAt: sess.StartedAt.Unix(),
	}
	if !sess.FinishedAt.IsZero() {
		r.FinishedAt = sess.FinishedAt.Unix()
	}
	return r, true
}

// gcLoop 定期清理已结束 / 过期的 session（保留 30 分钟便于前端最后一次取结果）。
func (s *KiroOAuthDeviceService) gcLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.mu.Lock()
		for id, sess := range s.sessions {
			sess.mu.Lock()
			finished := !sess.FinishedAt.IsZero() && now.Sub(sess.FinishedAt) > 30*time.Minute
			expired := now.After(sess.ExpiresAt) && now.Sub(sess.ExpiresAt) > 30*time.Minute
			sess.mu.Unlock()
			if finished || expired {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}
