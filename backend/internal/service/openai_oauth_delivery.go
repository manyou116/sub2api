package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const OpenAIOAuthDeliveryCapability = "openai_oauth_delivery_v1"

// OpenAIOAuthCredentialsRepository 只提交仍与刷新快照一致的 OpenAI 凭据，防止覆盖并发授权。
type OpenAIOAuthCredentialsRepository interface {
	UpdateOpenAIOAuthCredentialsIfUnchanged(context.Context, int64, map[string]any, *int64, map[string]any) (bool, error)
}

// OAuthRefreshLeaseCache 的 owner 保证旧执行者不能释放新租约。
type OAuthRefreshLeaseCache interface {
	AcquireRefreshLease(context.Context, string, time.Duration) (string, error)
	OwnsRefreshLease(context.Context, string, string) (bool, error)
	ReleaseRefreshLease(context.Context, string, string) error
}

// OpenAIOAuthCredentialSnapshot 仅在管理员专用接口返回明文凭据，revision 可用于后续条件写。
type OpenAIOAuthCredentialSnapshot struct {
	Capability         string         `json:"capability"`
	AccountID          int64          `json:"account_id"`
	CredentialRevision string         `json:"credential_revision"`
	Credentials        map[string]any `json:"credentials"`
	RefreshState       string         `json:"refresh_state"`
}

// oauthCoordinationUnavailable 返回固定脱敏错误，不泄漏 Redis 或数据库详情。
func oauthCoordinationUnavailable() error {
	return infraerrors.ServiceUnavailable("OAUTH_COORDINATION_UNAVAILABLE", "OAuth credential coordination unavailable")
}

// oauthCredentialConflict 要求调用者重新读取，禁止复用旧版本重试写入。
func oauthCredentialConflict() error {
	return infraerrors.Conflict("OAUTH_CREDENTIAL_CONFLICT", "OAuth credentials changed; read the current revision before retrying")
}

// openAICredentialRevision 覆盖实际凭据和刷新代理；不使用可复用的本地时钟充当 CAS 版本。
func openAICredentialRevision(account *Account) string {
	payload, _ := json.Marshal([]any{account.ID, account.Platform, account.Type, account.ProxyID, account.Credentials})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func openAICredentialSnapshot(account *Account) *OpenAIOAuthCredentialSnapshot {
	state := "ready"
	if account.GetCredential("_oauth_refresh_pending") != "" {
		state = "needs_reconciliation"
	}
	return &OpenAIOAuthCredentialSnapshot{Capability: OpenAIOAuthDeliveryCapability, AccountID: account.ID,
		CredentialRevision: openAICredentialRevision(account), Credentials: shallowCopyMap(account.Credentials), RefreshState: state}
}

func validateOpenAICredentialAccount(account *Account) error {
	if account == nil {
		return infraerrors.NotFound("ACCOUNT_NOT_FOUND", "Account not found")
	}
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || account.IsCredentialShadow() {
		return infraerrors.BadRequest("NOT_OPENAI_OAUTH", "A non-shadow OpenAI OAuth account is required")
	}
	return nil
}

// ReadOpenAIOAuthCredentials 能力仅在真实 CAS 和 owner 租约均可用时声明。
func (api *OAuthRefreshAPI) ReadOpenAIOAuthCredentials(ctx context.Context, id int64) (*OpenAIOAuthCredentialSnapshot, error) {
	if api == nil {
		return nil, oauthCoordinationUnavailable()
	}
	if _, ok := api.accountRepo.(OpenAIOAuthCredentialsRepository); !ok {
		return nil, oauthCoordinationUnavailable()
	}
	if _, ok := api.tokenCache.(OAuthRefreshLeaseCache); !ok {
		return nil, oauthCoordinationUnavailable()
	}
	account, err := api.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validateOpenAICredentialAccount(account); err != nil {
		return nil, err
	}
	if err := validateOpenAIDeliveryMode(account); err != nil {
		return nil, err
	}
	return openAICredentialSnapshot(account), nil
}

// acquireOpenAIRefreshLease 与后台刷新共用本地锁和 Redis 键；失败关闭，所有写入早于租约到期。
func (api *OAuthRefreshAPI) acquireOpenAIRefreshLease(ctx context.Context, account *Account) (context.Context, func() error, func(), error) {
	cache, ok := api.tokenCache.(OAuthRefreshLeaseCache)
	if !ok || api.lockTTL <= 5*time.Second {
		return nil, nil, nil, oauthCoordinationUnavailable()
	}
	key := OpenAITokenCacheKey(account)
	mu := api.getLocalLock(key)
	if err := mu.Lock(ctx); err != nil {
		return nil, nil, nil, err
	}
	started := time.Now()
	owner, err := cache.AcquireRefreshLease(ctx, key, api.lockTTL)
	if err != nil || owner == "" {
		mu.Unlock()
		if err != nil {
			return nil, nil, nil, oauthCoordinationUnavailable()
		}
		return nil, nil, nil, infraerrors.Conflict("OAUTH_REFRESH_BUSY", "OAuth credential operation is already running")
	}
	bounded, cancel := context.WithDeadline(ctx, started.Add(api.lockTTL-5*time.Second))
	check := func() error {
		if bounded.Err() != nil {
			return bounded.Err()
		}
		owns, err := cache.OwnsRefreshLease(bounded, key, owner)
		if err != nil || !owns {
			return oauthCoordinationUnavailable()
		}
		return nil
	}
	release := func() {
		cancel()
		cleanup, done := context.WithTimeout(context.Background(), defaultRefreshLockReleaseTimeout)
		defer done()
		_ = cache.ReleaseRefreshLease(cleanup, key, owner)
		mu.Unlock()
	}
	return bounded, check, release, nil
}

// MutateOpenAIOAuthCredentials 在同一租约内重读、核对版本、刷新或应用，然后 CAS 保存；不修改调度。
// 空 expected 仅供兼容管理入口使用，版本化 HTTP 接口必须先校验非空。
func (api *OAuthRefreshAPI) MutateOpenAIOAuthCredentials(ctx context.Context, id int64, expected string, credentials map[string]any, executor OAuthRefreshExecutor) (*Account, error) {
	if api == nil {
		return nil, oauthCoordinationUnavailable()
	}
	repo, ok := api.accountRepo.(OpenAIOAuthCredentialsRepository)
	if !ok {
		return nil, oauthCoordinationUnavailable()
	}
	bounded, check, release, err := api.acquireOpenAIRefreshLease(ctx, &Account{ID: id})
	if err != nil {
		return nil, err
	}
	defer release()
	account, err := api.accountRepo.GetByID(bounded, id)
	if err != nil {
		return nil, err
	}
	if err := validateOpenAICredentialAccount(account); err != nil {
		return nil, err
	}
	if expected != "" {
		if err := validateOpenAIDeliveryMode(account); err != nil {
			return nil, err
		}
	}
	if expected != "" && expected != openAICredentialRevision(account) {
		return nil, oauthCredentialConflict()
	}
	before := snapshotOAuthRefreshAccount(account)
	var next map[string]any
	if executor != nil {
		if strings.TrimSpace(account.GetOpenAIRefreshToken()) == "" || account.IsOpenAIPersonalAccessToken() {
			return nil, infraerrors.BadRequest("OPENAI_OAUTH_NO_REFRESH_TOKEN", "OAuth refresh token is required")
		}
		if err := api.prepareOpenAIRefresh(bounded, check, account); err != nil {
			return nil, err
		}
		before = snapshotOAuthRefreshAccount(account)
		next, err = executor.Refresh(context.WithValue(bounded, openAIExchangeOnlyKey{}, true), account)
		if err != nil {
			api.finishKnownOpenAIRefreshFailure(bounded, check, account, err)
			return nil, classifyOpenAIDeliveryRefreshError(err)
		}
		if credentialString(next, "access_token") == "" || credentialString(next, "refresh_token") == "" {
			return nil, infraerrors.New(502, "OAUTH_REFRESH_UNAVAILABLE", "OAuth refresh returned incomplete credentials; reconciliation required")
		}
	} else {
		if expected != "" && (strings.TrimSpace(credentialString(credentials, "access_token")) == "" || strings.TrimSpace(credentialString(credentials, "refresh_token")) == "") {
			return nil, infraerrors.BadRequest("OAUTH_CREDENTIALS_REQUIRED", "Both OAuth access and refresh tokens are required")
		}
		if expected != "" {
			for key := range credentials {
				switch key {
				case "access_token", "refresh_token", "id_token", "client_id", "expires_at", "expires_in", "chatgpt_account_id", "chatgpt_user_id", "organization_id", "plan_type", "subscription_expires_at", "email", "scope", "token_type":
				default:
					return nil, infraerrors.BadRequest("OAUTH_CREDENTIAL_FIELD_INVALID", "Unsupported OAuth credential field")
				}
			}
		}
		if replacement, _ := ctx.Value(openAICredentialReplacementKey{}).(bool); replacement {
			next = MergePreservingSensitiveCreds(account.Credentials, credentials)
			// UI 不能伪造/清除刷新在途标记与版本；身份字段未提交时也保留。
			for _, key := range []string{"_oauth_refresh_pending", "_token_version", "client_id", "chatgpt_account_id", "chatgpt_user_id", "expires_at"} {
				if strings.HasPrefix(key, "_") || credentials[key] == nil {
					if value, ok := account.Credentials[key]; ok {
						next[key] = value
					}
				}
			}
		} else {
			next = MergeCredentials(account.Credentials, shallowCopyMap(credentials))
		}
	}
	// 兼容编辑入口仍执行原凭据配置校验，不能因改走 CAS 绕过 header/protocol 约束。
	if executor == nil && expected == "" {
		if err := NormalizeHeaderOverrideCredentials(next); err != nil {
			return nil, err
		}
		if err := NormalizeProtocolRulesCredentials(next); err != nil {
			return nil, err
		}
	}
	if expected != "" && executor == nil {
		if err := validateOpenAIDeliveryIdentity(before, next); err != nil {
			return nil, err
		}
	}
	if err := check(); err != nil {
		return nil, err
	}
	replacement, _ := ctx.Value(openAICredentialReplacementKey{}).(bool)
	credentialWrite := expected != "" || !replacement
	for _, key := range []string{"access_token", "refresh_token", "id_token"} {
		if _, ok := credentials[key]; ok {
			credentialWrite = true
		}
	}
	if before.GetCredential("_oauth_refresh_pending") != "" && executor == nil && credentialWrite &&
		(credentialString(credentials, "refresh_token") == "" || credentialString(credentials, "refresh_token") == before.GetOpenAIRefreshToken()) {
		return nil, infraerrors.Conflict("OAUTH_REFRESH_RECONCILIATION_REQUIRED", "A fresh OAuth authorization is required to replace an unresolved refresh")
	}
	if executor != nil || credentialWrite {
		delete(next, "_oauth_refresh_pending")
	}
	next = SanitizeStoredCredentials(PlatformOpenAI, next)
	next["_token_version"] = time.Now().UnixMilli()
	applied, err := repo.UpdateOpenAIOAuthCredentialsIfUnchanged(bounded, id, before.Credentials, before.ProxyID, next)
	if err != nil {
		return nil, oauthCoordinationUnavailable()
	}
	if !applied {
		return nil, oauthCredentialConflict()
	}
	account.Credentials = next
	_ = api.tokenCache.DeleteAccessToken(bounded, OpenAITokenCacheKey(account))
	return account, nil
}

// credentialString 只接受字符串凭据，拒绝自动序列化意外结构。
func credentialString(credentials map[string]any, key string) string {
	value, _ := credentials[key].(string)
	return value
}

// OpenAIOAuthDeliveryAvailable 用于独立能力探测；不读取或输出账号凭据。
func (api *OAuthRefreshAPI) OpenAIOAuthDeliveryAvailable() bool {
	if api == nil {
		return false
	}
	_, cas := api.accountRepo.(OpenAIOAuthCredentialsRepository)
	_, lease := api.tokenCache.(OAuthRefreshLeaseCache)
	return cas && lease && api.lockTTL > 5*time.Second
}

// validateOpenAIDeliveryIdentity 只核对管理员提交材料的一致性，不把未验签 JWT 当授权依据。
func validateOpenAIDeliveryIdentity(before *Account, next map[string]any) error {
	invalid := infraerrors.BadRequest("OAUTH_CREDENTIAL_IDENTITY_INVALID", "OAuth access token identity, client, and expiry must match the account")
	parts := strings.Split(credentialString(next, "access_token"), ".")
	if len(parts) != 3 {
		return invalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return invalid
	}
	var claims struct {
		Exp      int64  `json:"exp"`
		ClientID string `json:"client_id"`
		AZP      string `json:"azp"`
		Auth     struct {
			AccountID string `json:"chatgpt_account_id"`
			UserID    string `json:"chatgpt_user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= time.Now().Unix() || claims.Auth.AccountID == "" {
		return invalid
	}
	if credentialString(next, "refresh_token") == "" || credentialString(next, "client_id") == "" {
		return invalid
	}
	// 老记录可能只有 AT 而没有独立身份字段；仍必须与原 AT 的账号身份一致。
	oldAccountID := before.GetCredential("chatgpt_account_id")
	oldUserID := before.GetCredential("chatgpt_user_id")
	oldParts := strings.Split(before.GetCredential("access_token"), ".")
	if len(oldParts) == 3 {
		var oldClaims struct {
			Auth struct {
				AccountID string `json:"chatgpt_account_id"`
				UserID    string `json:"chatgpt_user_id"`
			} `json:"https://api.openai.com/auth"`
		}
		if raw, err := base64.RawURLEncoding.DecodeString(oldParts[1]); err == nil && json.Unmarshal(raw, &oldClaims) == nil {
			if oldAccountID == "" {
				oldAccountID = oldClaims.Auth.AccountID
			}
			if oldUserID == "" {
				oldUserID = oldClaims.Auth.UserID
			}
		}
	}
	if oldAccountID == "" || oldAccountID != claims.Auth.AccountID || (oldUserID != "" && oldUserID != claims.Auth.UserID) {
		return invalid
	}
	for key, identity := range map[string]string{"chatgpt_account_id": claims.Auth.AccountID, "chatgpt_user_id": claims.Auth.UserID} {
		if value := credentialString(next, key); value != "" && value != identity {
			return invalid
		}
		if value := before.GetCredential(key); value != "" && value != identity {
			return invalid
		}
		if identity != "" {
			next[key] = identity
		}
	}
	clientID := credentialString(next, "client_id")
	if (claims.ClientID != "" && claims.ClientID != clientID) || (claims.AZP != "" && claims.AZP != clientID) {
		return invalid
	}
	next["expires_at"] = time.Unix(claims.Exp, 0).UTC().Format(time.RFC3339)
	return nil
}

// classifyOpenAIDeliveryRefreshError 只有明确的上游 OAuth 错误允许重登；网络和限流不会误报 RT 失效。
func classifyOpenAIDeliveryRefreshError(err error) error {
	var app *infraerrors.ApplicationError
	if errors.As(err, &app) && app.Reason == "OPENAI_OAUTH_TOKEN_REFRESH_FAILED" {
		message := app.Message
		if strings.Contains(message, "status 429,") {
			return infraerrors.TooManyRequests("OAUTH_REFRESH_RATE_LIMITED", "OAuth refresh rate limited")
		}
		start := strings.Index(message, "body: ")
		var body struct {
			Error json.RawMessage `json:"error"`
			Code  string          `json:"code"`
		}
		if start >= 0 && json.Unmarshal([]byte(message[start+6:]), &body) == nil {
			code := body.Code
			var text string
			var nested struct {
				Code string `json:"code"`
				Type string `json:"type"`
			}
			if json.Unmarshal(body.Error, &text) == nil {
				code = text
			} else if json.Unmarshal(body.Error, &nested) == nil {
				code = nested.Code
				if code == "" {
					code = nested.Type
				}
			}
			switch code {
			case "account_deactivated", "account_disabled", "user_deactivated":
				return infraerrors.Forbidden("OAUTH_ACCOUNT_DEACTIVATED", "OAuth account is deactivated")
			case "invalid_grant", "refresh_token_reused", "refresh_token_expired", "refresh_token_revoked":
				return infraerrors.BadRequest("OAUTH_REFRESH_TOKEN_INVALID", "OAuth refresh token is invalid")
			}
		}
	}
	return infraerrors.New(502, "OAUTH_REFRESH_UNAVAILABLE", "OAuth refresh did not complete; read current credentials before retrying")
}

type preserveOpenAICredentialsKey struct{}

// PreserveOpenAICredentialsUpdate 防止只改分组的旧快照覆盖并发刷新后的凭据。
func PreserveOpenAICredentialsUpdate(ctx context.Context) context.Context {
	return context.WithValue(ctx, preserveOpenAICredentialsKey{}, true)
}

// PreservesOpenAICredentials 供 repository 读取本次更新的凭据保留意图。
func PreservesOpenAICredentials(ctx context.Context) bool {
	value, _ := ctx.Value(preserveOpenAICredentialsKey{}).(bool)
	return value
}

// validateOpenAIDeliveryMode 不把 Identity/PAT 账号静默转换成 OAuth，避免保留原认证分支。
func validateOpenAIDeliveryMode(account *Account) error {
	for _, key := range []string{"auth_mode", "openai_auth_mode"} {
		mode := strings.ToLower(strings.TrimSpace(account.GetCredential(key)))
		if mode != "" && mode != "oauth" {
			return infraerrors.BadRequest("NOT_OPENAI_OAUTH", "The remote credential mode is not OAuth refresh-token mode")
		}
	}
	return nil
}

// prepareOpenAIRefresh 先持久化在途标记；租约过期或进程崩溃后不得再次消费同一个 RT。
func (api *OAuthRefreshAPI) prepareOpenAIRefresh(ctx context.Context, check func() error, account *Account) error {
	if account.GetCredential("_oauth_refresh_pending") != "" {
		return infraerrors.Conflict("OAUTH_REFRESH_RECONCILIATION_REQUIRED", "Previous OAuth refresh requires credential reconciliation or new authorization")
	}
	if err := check(); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return oauthCoordinationUnavailable()
	}
	next := shallowCopyMap(account.Credentials)
	next["_oauth_refresh_pending"] = hex.EncodeToString(nonce[:])
	applied, err := api.accountRepo.(OpenAIOAuthCredentialsRepository).UpdateOpenAIOAuthCredentialsIfUnchanged(ctx, account.ID, account.Credentials, account.ProxyID, next)
	if err != nil {
		return oauthCoordinationUnavailable()
	}
	if !applied {
		return oauthCredentialConflict()
	}
	account.Credentials = next
	return check()
}

// finishKnownOpenAIRefreshFailure 只有明确拒绝且未返回新凭据时清在途标记；传输错误继续待确认。
func (api *OAuthRefreshAPI) finishKnownOpenAIRefreshFailure(ctx context.Context, check func() error, account *Account, refreshErr error) {
	reason := infraerrors.Reason(classifyOpenAIDeliveryRefreshError(refreshErr))
	if reason != "OAUTH_REFRESH_TOKEN_INVALID" && reason != "OAUTH_ACCOUNT_DEACTIVATED" && reason != "OAUTH_REFRESH_RATE_LIMITED" {
		return
	}
	if check() != nil {
		return
	}
	next := shallowCopyMap(account.Credentials)
	delete(next, "_oauth_refresh_pending")
	_, _ = api.accountRepo.(OpenAIOAuthCredentialsRepository).UpdateOpenAIOAuthCredentialsIfUnchanged(ctx, account.ID, account.Credentials, account.ProxyID, next)
}

type openAICredentialReplacementKey struct{}

// UpdateOpenAIOAuthCredentialSettings 保留旧编辑页非敏感配置的删除语义，并共享凭据互斥。
func (api *OAuthRefreshAPI) UpdateOpenAIOAuthCredentialSettings(ctx context.Context, id int64, credentials map[string]any) (*Account, error) {
	return api.MutateOpenAIOAuthCredentials(context.WithValue(ctx, openAICredentialReplacementKey{}, true), id, "", credentials, nil)
}

type openAIExchangeOnlyKey struct{}

// openAIExchangeOnly 将纯 token 交换与网页补全分开，新 RT 必须先可靠保存。
func openAIExchangeOnly(ctx context.Context) bool {
	enabled, _ := ctx.Value(openAIExchangeOnlyKey{}).(bool)
	return enabled
}
