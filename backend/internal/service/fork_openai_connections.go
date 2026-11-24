// SIWC protocol adapted from ranxi2001/sub2api (e90811e), LGPL-3.0.
package service

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const siwcConnectionKind = "siwc"
const excelConnectionKind = "excel"
const excelBPSSearchKey = "fork_excel_bps_search"
const excelBPSModelsKey = "fork_excel_bps_models"
const openAIConnectionProtocolKey = "fork_openai_connection_protocol"
const OpenAISIWCEnabledKey = "openai_siwc"

type ForkOpenAIConnection struct {
	HostID     string
	Ciphertext string
	Revision   int64
}

type forkOpenAIConnectionRepository interface {
	EnsureOpenAIConnection(context.Context, int64, string, string) error
	ReadOpenAIConnection(context.Context, int64, string) (ForkOpenAIConnection, error)
	SaveOpenAIConnection(context.Context, int64, string, int64, string, string, string) error
}

type siwcTokenClient interface {
	SIWCToken(context.Context, url.Values, string, string) (*openai.TokenResponse, *openai.SIWCIdentity, error)
}

type forkOpenAIConnections struct {
	accounts  AccountRepository
	repo      forkOpenAIConnectionRepository
	encryptor SecretEncryptor
	redis     *redis.Client
}

func (a *Account) IsSIWCEnabled() bool {
	return a.connectionProtocol() == siwcConnectionKind
}

func (a *Account) connectionProtocol() string {
	if !eligibleSIWCAccount(a) {
		return ""
	}
	switch a.Extra[openAIConnectionProtocolKey] {
	case "siwc":
		return siwcConnectionKind
	case "excel":
		return excelConnectionKind
	case "codex":
		return ""
	}
	if a.Extra[OpenAISIWCEnabledKey] == true {
		return siwcConnectionKind
	}
	return ""
}

func eligibleSIWCAccount(a *Account) bool {
	return a != nil && a.IsOpenAIOAuth() && !a.IsCredentialShadow() && !a.IsOpenAIAgentIdentity() && !a.IsOpenAIPersonalAccessToken()
}

type siwcGrant struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ClientID     string
	Subject      string
	Email        string
	Workspace    string
	ExpiresAt    time.Time
}

type siwcAuthSession struct {
	AccountID int64
	Revision  int64
	Email     string
	Workspace string
	ClientID  string
	State     string
	Verifier  string
	Nonce     string
	Proxy     string
}

type SIWCStatus struct {
	Bound         bool      `json:"bound"`
	Enabled       bool      `json:"enabled"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	SearchEnabled bool      `json:"search_enabled,omitempty"`
	Models        []string  `json:"models,omitempty"`
}

func (s *OpenAIOAuthService) connectionAccount(ctx context.Context, id int64) (*Account, error) {
	if s == nil || s.connections == nil || s.connections.repo == nil || s.connections.redis == nil || s.connections.encryptor == nil {
		return nil, infraerrors.ServiceUnavailable("OPENAI_SIWC_UNAVAILABLE", "OpenAI authorization storage is unavailable")
	}
	a, err := s.connections.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !eligibleSIWCAccount(a) || strings.TrimSpace(a.GetCredential("email")) == "" {
		return nil, infraerrors.BadRequest("OPENAI_SIWC_ACCOUNT", "SIWC requires an OpenAI OAuth parent with a stored email")
	}
	return a, nil
}

func (s *OpenAIOAuthService) loadOpenAIConnectionGrant(ctx context.Context, id int64, kind string) (ForkOpenAIConnection, siwcGrant, error) {
	record, err := s.connections.repo.ReadOpenAIConnection(ctx, id, kind)
	var grant siwcGrant
	if err != nil || record.Ciphertext == "" {
		return record, grant, err
	}
	plain, err := s.connections.encryptor.Decrypt(record.Ciphertext)
	if err != nil || json.Unmarshal([]byte(plain), &grant) != nil {
		return record, grant, errors.New("failed to read SIWC grant")
	}
	return record, grant, nil
}

func siwcStoredIdentityMatches(a *Account, email, workspace string) bool {
	return email != "" && strings.EqualFold(strings.TrimSpace(a.GetCredential("email")), email) &&
		a.GetCredential("chatgpt_account_id") == workspace
}

func (s *OpenAIOAuthService) GetOpenAIConnectionStatus(ctx context.Context, id int64, kind string) (SIWCStatus, error) {
	a, err := s.connectionAccount(ctx, id)
	if err != nil {
		return SIWCStatus{}, err
	}
	_, grant, err := s.loadOpenAIConnectionGrant(ctx, id, kind)
	var models []string
	if kind == excelConnectionKind {
		models = excelBPSConfiguredModels(a)
	}
	return SIWCStatus{
		Models:  models,
		Bound:   grant.RefreshToken != "" && siwcStoredIdentityMatches(a, grant.Email, grant.Workspace),
		Enabled: a.connectionProtocol() == kind, ExpiresAt: grant.ExpiresAt,
		SearchEnabled: kind == excelConnectionKind && a.Extra[excelBPSSearchKey] == true,
	}, err
}

func (s *OpenAIOAuthService) connectionProxy(ctx context.Context, account *Account) (string, error) {
	if account.ProxyID == nil {
		return "", nil
	}
	p, err := s.proxyRepo.GetByID(ctx, *account.ProxyID)
	if err != nil || p == nil {
		return "", errors.New("OpenAI authorization proxy is unavailable")
	}
	return p.URL(), nil
}

func (s *OpenAIOAuthService) saveOpenAIConnectionGrant(ctx context.Context, id int64, kind string, revision int64, grant siwcGrant) error {
	raw, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	encrypted, err := s.connections.encryptor.Encrypt(string(raw))
	if err != nil {
		return errors.New("failed to encrypt SIWC grant")
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.connections.repo.SaveOpenAIConnection(persist, id, kind, revision, grant.Email, grant.Workspace, encrypted)
}

func (s *OpenAIOAuthService) BeginOpenAIConnectionAuthorization(ctx context.Context, id int64, kind string) (*OpenAIAuthURLResult, error) {
	a, err := s.connectionAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	proxy, err := s.connectionProxy(ctx, a)
	if err != nil {
		return nil, err
	}
	if err = s.connections.repo.EnsureOpenAIConnection(ctx, id, kind, "urn:uuid:"+uuid.NewString()); err != nil {
		return nil, err
	}
	record, grant, err := s.loadOpenAIConnectionGrant(ctx, id, kind)
	if err != nil {
		return nil, err
	}
	if record.HostID == "" {
		return nil, errors.New("SIWC host registration is unavailable")
	}
	email := strings.TrimSpace(a.GetCredential("email"))
	workspace := a.GetCredential("chatgpt_account_id")
	if grant.ClientID != "" && !siwcStoredIdentityMatches(a, grant.Email, grant.Workspace) {
		return nil, infraerrors.Conflict("OPENAI_SIWC_ACCOUNT_CHANGED", "SIWC grant belongs to a different local account identity")
	}
	verifier, err := openai.GenerateCodeVerifier()
	if err != nil {
		return nil, err
	}
	state, err := openai.GenerateState()
	if err != nil {
		return nil, err
	}
	nonce, err := openai.GenerateState()
	if err != nil {
		return nil, err
	}
	idToken, err := openai.GenerateSessionID()
	if err != nil {
		return nil, err
	}
	session := siwcAuthSession{AccountID: id, Revision: record.Revision, Email: email, Workspace: workspace, ClientID: grant.ClientID, State: state, Verifier: verifier, Nonce: nonce, Proxy: proxy}
	raw, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}
	encrypted, err := s.connections.encryptor.Encrypt(string(raw))
	if err != nil {
		return nil, errors.New("failed to encrypt SIWC authorization session")
	}
	if err = s.connections.redis.Set(ctx, siwcSessionKey(id, kind, idToken), encrypted, 30*time.Minute).Err(); err != nil {
		return nil, errors.New("SIWC authorization session storage unavailable")
	}
	challenge := openai.GenerateCodeChallenge(verifier)
	authURL := openai.BuildSIWCAuthorizationURL(state, challenge, nonce, record.HostID, grant.ClientID, grant.IDToken, email)
	if kind == excelConnectionKind {
		authURL = openai.BuildExcelAuthorizationURL(state, challenge)
	}
	return &OpenAIAuthURLResult{SessionID: idToken, AuthURL: authURL}, nil
}

func siwcSessionKey(id int64, kind, sessionID string) string {
	return "fork:openai:" + kind + ":session:" + strconv.FormatInt(id, 10) + ":" + sessionID
}

var takeSIWCSession = redis.NewScript(`
	if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end
	return 0
`)

func parseOpenAIConnectionCallback(callback, state, originalClient, kind string) (code, clientID string, err error) {
	fail := infraerrors.BadRequest("OPENAI_SIWC_CALLBACK_INVALID", "Invalid SIWC callback")
	if len(callback) > 16384 {
		return "", "", fail
	}
	u, err := url.Parse(callback)
	redirect, _ := url.Parse(connectionRedirect(kind))
	if err != nil || u.Scheme != redirect.Scheme || u.Host != redirect.Host ||
		u.Path != redirect.Path || u.RawPath != "" || u.User != nil || u.Fragment != "" {
		return "", "", fail
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", "", fail
	}
	for _, key := range []string{"state", "code", "client_id", "error"} {
		if len(q[key]) > 1 {
			return "", "", fail
		}
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
		return "", "", fail
	}
	code, clientID = q.Get("code"), q.Get("client_id")
	if kind == excelConnectionKind {
		originalClient = openai.ExcelClientID
	}
	if originalClient != "" {
		if clientID != "" && clientID != originalClient {
			return "", "", fail
		}
		clientID = originalClient
	}
	if q.Get("error") != "" || code == "" || clientID == "" || clientID == openai.SIWCDynamicClientID ||
		(kind == siwcConnectionKind && (clientID == openai.ExcelClientID || clientID == openai.ClientID)) {
		return "", "", fail
	}
	return code, clientID, nil
}

func (s *OpenAIOAuthService) CompleteOpenAIConnectionAuthorization(ctx context.Context, id int64, kind, sessionID, callback string) error {
	a, err := s.connectionAccount(ctx, id)
	if err != nil {
		return err
	}
	if len(sessionID) == 0 || len(sessionID) > 128 {
		return infraerrors.BadRequest("OPENAI_SIWC_SESSION_INVALID", "Invalid SIWC authorization session")
	}
	key := siwcSessionKey(id, kind, sessionID)
	ciphertext, err := s.connections.redis.Get(ctx, key).Result()
	if err != nil {
		return infraerrors.BadRequest("OPENAI_SIWC_SESSION_INVALID", "SIWC authorization session expired or unavailable")
	}
	plain, err := s.connections.encryptor.Decrypt(ciphertext)
	var session siwcAuthSession
	if err != nil || json.Unmarshal([]byte(plain), &session) != nil || session.AccountID != id ||
		!siwcStoredIdentityMatches(a, session.Email, session.Workspace) {
		return infraerrors.BadRequest("OPENAI_SIWC_SESSION_INVALID", "SIWC authorization session no longer matches the account")
	}
	code, clientID, err := parseOpenAIConnectionCallback(callback, session.State, session.ClientID, kind)
	if err != nil {
		return err
	}
	record, existing, err := s.loadOpenAIConnectionGrant(ctx, id, kind)
	if err != nil {
		return err
	}
	if record.Revision != session.Revision {
		return infraerrors.Conflict("OPENAI_SIWC_ACCOUNT_CHANGED", "SIWC registration changed during authorization")
	}
	client, ok := s.oauthClient.(siwcTokenClient)
	if !ok {
		return errors.New("SIWC token client is unavailable")
	}
	n, err := takeSIWCSession.Run(ctx, s.connections.redis, []string{key}, ciphertext).Int()
	if err != nil || n != 1 {
		return infraerrors.Conflict("OPENAI_SIWC_SESSION_CONSUMED", "SIWC authorization session already used or unavailable")
	}
	exchange, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	token, identity, err := client.SIWCToken(exchange, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code},
		"code_verifier": {session.Verifier}, "redirect_uri": {connectionRedirect(kind)},
	}, session.Proxy, session.Nonce)
	if err != nil {
		return err
	}
	if identity == nil || token == nil || token.RefreshToken == "" ||
		(kind == excelConnectionKind && !matchesExcelWorkspace(token, session.Workspace)) ||
		!strings.EqualFold(strings.TrimSpace(identity.Email), session.Email) ||
		(existing.Subject != "" && existing.Subject != identity.Subject) {
		return infraerrors.BadRequest("OPENAI_SIWC_IDENTITY_MISMATCH", "SIWC authorization does not match the account owner")
	}
	return s.saveOpenAIConnectionGrant(ctx, id, kind, record.Revision, siwcGrant{
		AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, IDToken: token.IDToken, ClientID: clientID,
		Subject: identity.Subject, Email: session.Email, Workspace: session.Workspace,
		ExpiresAt: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second),
	})
}

func (s *OpenAIOAuthService) openAIConnectionToken(ctx context.Context, id int64, kind string) (string, error) {
	a, err := s.connectionAccount(ctx, id)
	if err != nil {
		return "", err
	}
	if a.connectionProtocol() != kind {
		return "", infraerrors.Conflict("OPENAI_SIWC_SELECTION_CHANGED", "OpenAI request protocol changed")
	}
	record, grant, err := s.loadOpenAIConnectionGrant(ctx, id, kind)
	if err != nil {
		return "", err
	}
	if grant.RefreshToken == "" || !siwcStoredIdentityMatches(a, grant.Email, grant.Workspace) {
		return "", infraerrors.BadRequest("OPENAI_SIWC_AUTH_REQUIRED", "Authorize SIWC for this account first")
	}
	if time.Until(grant.ExpiresAt) > openAITokenRefreshSkew {
		return grant.AccessToken, nil
	}
	key := fmt.Sprintf("fork:openai:%s:refresh:%d", kind, id)
	owner := uuid.NewString()
	ok, err := s.connections.redis.SetNX(ctx, key, owner, time.Minute).Result()
	if err != nil || !ok {
		return "", infraerrors.ServiceUnavailable("OPENAI_SIWC_REFRESH_BUSY", "SIWC refresh is in progress or unavailable")
	}
	defer func() {
		release, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = takeSIWCSession.Run(release, s.connections.redis, []string{key}, owner).Err()
	}()

	current, refreshed, err := s.loadOpenAIConnectionGrant(ctx, id, kind)
	if err != nil {
		return "", err
	}
	if current.Revision != record.Revision {
		if siwcStoredIdentityMatches(a, refreshed.Email, refreshed.Workspace) && time.Until(refreshed.ExpiresAt) > openAITokenRefreshSkew {
			return refreshed.AccessToken, nil
		}
		return "", infraerrors.Conflict("OPENAI_SIWC_GRANT_CHANGED", "SIWC grant changed before refresh")
	}
	client, ok := s.oauthClient.(siwcTokenClient)
	if !ok {
		return "", errors.New("SIWC token client is unavailable")
	}
	proxy, err := s.connectionProxy(ctx, a)
	if err != nil {
		return "", err
	}
	refresh, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	token, identity, err := client.SIWCToken(refresh, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {grant.ClientID}, "refresh_token": {grant.RefreshToken},
	}, proxy, "")
	if err != nil {
		return "", err
	}
	if token == nil || (kind == excelConnectionKind && !matchesExcelWorkspace(token, grant.Workspace)) ||
		(identity != nil && (identity.Subject != grant.Subject || !strings.EqualFold(identity.Email, grant.Email))) {
		return "", errors.New("SIWC refresh changed the authorized identity")
	}
	grant.AccessToken = token.AccessToken
	grant.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	if token.RefreshToken != "" {
		grant.RefreshToken = token.RefreshToken
	}
	if token.IDToken != "" {
		grant.IDToken = token.IDToken
	}
	if err = s.saveOpenAIConnectionGrant(ctx, id, kind, record.Revision, grant); err != nil {
		return "", err
	}
	return grant.AccessToken, nil
}

// Expire only the rejected grant if it is still current; a late 401 must not
// invalidate the replacement. The primary Codex token is independent.
func (s *OpenAIOAuthService) invalidateOpenAIConnectionToken(ctx context.Context, id int64, kind, token string) {
	if s == nil || s.connections == nil || token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	record, grant, err := s.loadOpenAIConnectionGrant(ctx, id, kind)
	if err == nil && grant.AccessToken == token && !grant.ExpiresAt.IsZero() {
		grant.ExpiresAt = time.Time{}
		_ = s.saveOpenAIConnectionGrant(ctx, id, kind, record.Revision, grant)
	}
}

func (s *OpenAIOAuthService) SetOpenAIConnectionEnabled(ctx context.Context, id int64, kind string, enabled bool) error {
	status, err := s.GetOpenAIConnectionStatus(ctx, id, kind)
	if err != nil {
		return err
	}
	if enabled && !status.Bound {
		return infraerrors.BadRequest("OPENAI_SIWC_AUTH_REQUIRED", "Authorize SIWC for this account first")
	}
	protocol := kind
	if !enabled {
		protocol = "codex"
		if !status.Enabled {
			return nil
		}
	}
	return s.connections.accounts.UpdateExtra(ctx, id, map[string]any{openAIConnectionProtocolKey: protocol, OpenAISIWCEnabledKey: false})
}

func connectionRedirect(kind string) string {
	if kind == excelConnectionKind {
		return openai.ExcelRedirectURI
	}
	return openai.SIWCRedirectURI
}

func matchesExcelWorkspace(token *openai.TokenResponse, workspace string) bool {
	// The token is taken only from the fixed OAuth endpoint. Its ID token, when
	// present, is independently verified before this credential bundle is used.
	access, err := openai.DecodeIDToken(token.AccessToken)
	return err == nil && access.OpenAIAuth != nil && workspace != "" && access.OpenAIAuth.ChatGPTAccountID == workspace
}

// Check the selected grant at the last send boundary, after asynchronous
// adapters and refresh. A disabled connection or a rotated grant may not send.
func (s *OpenAIOAuthService) checkOpenAIConnectionEgress(ctx context.Context, id int64, kind, token string) error {
	account, err := s.connectionAccount(ctx, id)
	if err != nil {
		return err
	}
	_, grant, err := s.loadOpenAIConnectionGrant(ctx, id, kind)
	if err != nil {
		return err
	}
	if account.connectionProtocol() != kind || token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(grant.AccessToken)) != 1 ||
		!siwcStoredIdentityMatches(account, grant.Email, grant.Workspace) || !time.Now().Before(grant.ExpiresAt) {
		return infraerrors.Conflict("OPENAI_CONNECTION_CHANGED", "OpenAI authorization changed before sending")
	}
	return nil
}

func (s *OpenAIOAuthService) SetExcelBPSSearchEnabled(ctx context.Context, id int64, enabled bool) error {
	status, err := s.GetOpenAIConnectionStatus(ctx, id, excelConnectionKind)
	if err != nil {
		return err
	}
	if !status.Bound {
		return infraerrors.BadRequest("OPENAI_EXCEL_AUTH_REQUIRED", "Authorize Excel BPS for this account first")
	}
	return s.connections.accounts.UpdateExtra(ctx, id, map[string]any{excelBPSSearchKey: enabled})
}

// The connection control owns these settings. Generic edits routinely send a
// stale whole Extra object for unrelated changes.
func preserveOpenAIConnectionSelection(current, update map[string]any) {
	for _, key := range []string{openAIConnectionProtocolKey, OpenAISIWCEnabledKey, excelBPSSearchKey, excelBPSModelsKey} {
		if value, ok := current[key]; ok {
			update[key] = value
		} else {
			delete(update, key)
		}
	}
}

// Clear the local grant and advance its revision. An in-flight refresh or
// authorization must not be able to restore a discarded credential.
func (s *OpenAIOAuthService) RemoveOpenAIConnection(ctx context.Context, id int64, kind string) error {
	a, err := s.connectionAccount(ctx, id)
	if err != nil {
		return err
	}
	record, err := s.connections.repo.ReadOpenAIConnection(ctx, id, kind)
	if err != nil {
		return err
	}
	if record.HostID == "" {
		return nil
	}
	if err = s.connections.repo.SaveOpenAIConnection(ctx, id, kind, record.Revision,
		strings.TrimSpace(a.GetCredential("email")), a.GetCredential("chatgpt_account_id"), ""); err != nil {
		return err
	}
	if a.connectionProtocol() == kind {
		return s.connections.accounts.UpdateExtra(ctx, id, map[string]any{
			openAIConnectionProtocolKey: "codex", OpenAISIWCEnabledKey: false,
		})
	}
	return nil
}

func (s *OpenAIOAuthService) SetExcelBPSModels(ctx context.Context, id int64, models []string) error {
	if _, err := s.connectionAccount(ctx, id); err != nil {
		return err
	}
	if len(models) > 128 {
		return infraerrors.BadRequest("OPENAI_EXCEL_MODEL_LIMIT", "Choose up to 128 Excel BPS models")
	}
	valid := make([]string, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, item := range models {
		name := strings.TrimSpace(item)
		if len(name) > 128 || strings.ContainsAny(name, " \t\r\n*") {
			return infraerrors.BadRequest("OPENAI_EXCEL_MODEL_INVALID", "Excel BPS model identifiers must be exact names")
		}
		if name != "" && !seen[name] {
			valid = append(valid, name)
			seen[name] = true
		}
	}
	return s.connections.accounts.UpdateExtra(ctx, id, map[string]any{excelBPSModelsKey: valid})
}
