//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type siwcTestTransport struct {
	HTTPUpstream
	request *http.Request
}

func (u *siwcTestTransport) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.request = req
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBuffer(nil))}, nil
}

func TestSIWCForkOutboundIsolation(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Session-Id", "local-session")
	c.Request.Header.Set("X-Codex-Turn-State", "incoming")
	c.Request.Header.Set("Cookie", "incoming")
	a := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAISIWCEnabledKey: true}}
	u := &siwcTestTransport{}
	s := &OpenAIGatewayService{httpUpstream: u}
	req, err := s.buildUpstreamRequest(context.Background(), c, a, []byte(`{"input":"hi","previous_response_id":"old","turn_state":"old","client_metadata":{"a":"b"}}`), "grant", false, "", false)
	require.NoError(t, err)
	req.Header.Set("X-Codex-Turn-State", "late")
	req.Header.Set("Cookie", "late")
	resp, err := s.doOpenAIUpstream(req, "", a)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, "https://api.openai.com/v1/responses", u.request.URL.String())
	require.Equal(t, "Bearer grant", u.request.Header.Get("Authorization"))
	require.Equal(t, "text/event-stream", u.request.Header.Get("Accept"))
	require.Empty(t, u.request.Header.Get("Cookie"))
	require.Empty(t, u.request.Header.Get("X-Codex-Turn-State"))
	require.NotEmpty(t, u.request.Header.Get("session_id"))
	require.NotEqual(t, "local-session", u.request.Header.Get("session_id"))
	data, err := io.ReadAll(u.request.Body)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(data, "stream").Bool())
	require.False(t, gjson.GetBytes(data, "store").Bool())
	require.False(t, gjson.GetBytes(data, "previous_response_id").Exists())
	require.False(t, gjson.GetBytes(data, "turn_state").Exists())
	require.False(t, gjson.GetBytes(data, "client_metadata").Exists())
}

func TestSIWCForkCallbackBinding(t *testing.T) {
	code, client, err := parseOpenAIConnectionCallback("http://127.0.0.1:1455/auth/callback?code=ok&state=expected&client_id=issued", "expected", "", siwcConnectionKind)
	require.NoError(t, err)
	require.Equal(t, "ok", code)
	require.Equal(t, "issued", client)
	for _, input := range []string{
		"http://127.0.0.1:1455/auth/callback?code=ok&state=bad&client_id=issued",
		"http://127.0.0.1:1455/auth/callback?code=ok&state=expected&client_id=other",
		"http://127.0.0.1:1455/auth/callback?code=ok&code=other&state=expected&client_id=issued",
		"http://localhost:1455/auth/callback?code=ok&state=expected&client_id=issued",
	} {
		_, _, err := parseOpenAIConnectionCallback(input, "expected", "issued", siwcConnectionKind)
		require.Error(t, err)
	}
}

type siwcLifecycleRepo struct {
	AccountRepository
	account *Account
	records map[string]ForkOpenAIConnection
}

func (r *siwcLifecycleRepo) GetByID(context.Context, int64) (*Account, error) { return r.account, nil }

func (r *siwcLifecycleRepo) EnsureOpenAIConnection(_ context.Context, _ int64, kind, host string) error {
	if _, exists := r.records[kind]; !exists {
		r.records[kind] = ForkOpenAIConnection{HostID: host}
	}
	return nil
}

func (r *siwcLifecycleRepo) ReadOpenAIConnection(_ context.Context, _ int64, kind string) (ForkOpenAIConnection, error) {
	return r.records[kind], nil
}

func (r *siwcLifecycleRepo) SaveOpenAIConnection(_ context.Context, _ int64, kind string, rev int64, email, workspace, ciphertext string) error {
	current := r.records[kind]
	if current.Revision != rev || !siwcStoredIdentityMatches(r.account, email, workspace) {
		return errors.New("stale authorization")
	}
	current.Ciphertext = ciphertext
	current.Revision++
	r.records[kind] = current
	return nil
}

func (r *siwcLifecycleRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	for key, value := range updates {
		r.account.Extra[key] = value
	}
	return nil
}

type siwcLifecycleTokenClient struct {
	OpenAIOAuthClient
	exchange func(url.Values, string) (*openai.TokenResponse, *openai.SIWCIdentity, error)
}

func (c *siwcLifecycleTokenClient) SIWCToken(_ context.Context, values url.Values, _ string, nonce string) (*openai.TokenResponse, *openai.SIWCIdentity, error) {
	return c.exchange(values, nonce)
}

func TestSIWCForkLifecycleIsolation(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	account := &Account{
		ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{},
		Credentials: map[string]any{"email": "user@example.test", "chatgpt_account_id": "workspace", "access_token": "primary"},
	}
	repo := &siwcLifecycleRepo{account: account, records: make(map[string]ForkOpenAIConnection)}
	cipher := &liveAttestationAES{key: [32]byte{3}}
	token := &siwcLifecycleTokenClient{}
	service := &OpenAIOAuthService{oauthClient: token, connections: &forkOpenAIConnections{accounts: repo, repo: repo, redis: redisClient, encryptor: cipher}}
	flow, err := service.BeginOpenAIConnectionAuthorization(ctx, account.ID, siwcConnectionKind)
	require.NoError(t, err)
	auth, err := url.Parse(flow.AuthURL)
	require.NoError(t, err)
	state := auth.Query().Get("state")
	var exchanges int
	token.exchange = func(form url.Values, nonce string) (*openai.TokenResponse, *openai.SIWCIdentity, error) {
		exchanges++
		require.Equal(t, auth.Query().Get("nonce"), nonce)
		require.Equal(t, auth.Query().Get("code_challenge"), openai.GenerateCodeChallenge(form.Get("code_verifier")))
		require.Equal(t, "fixture", form.Get("code"))
		return &openai.TokenResponse{AccessToken: "first-grant", RefreshToken: "refresh-grant", ExpiresIn: 3600},
			&openai.SIWCIdentity{RegisteredClaims: jwt.RegisteredClaims{Subject: "subscriber"}, Email: "user@example.test"}, nil
	}
	callback := openai.SIWCRedirectURI + "?code=fixture&client_id=issued&state=" + url.QueryEscape(state)
	require.Error(t, service.CompleteOpenAIConnectionAuthorization(ctx, account.ID, excelConnectionKind, flow.SessionID, callback))
	require.NoError(t, service.CompleteOpenAIConnectionAuthorization(ctx, account.ID, siwcConnectionKind, flow.SessionID, callback))
	require.Error(t, service.CompleteOpenAIConnectionAuthorization(ctx, account.ID, siwcConnectionKind, flow.SessionID, callback))
	require.Equal(t, 1, exchanges)
	require.False(t, strings.Contains(repo.records[siwcConnectionKind].Ciphertext, "refresh-grant"))
	require.False(t, account.IsSIWCEnabled())
	require.NoError(t, service.SetOpenAIConnectionEnabled(ctx, account.ID, siwcConnectionKind, true))
	value, err := service.openAIConnectionToken(ctx, account.ID, siwcConnectionKind)
	require.NoError(t, err)
	require.Equal(t, "first-grant", value)
	service.invalidateOpenAIConnectionToken(ctx, account.ID, siwcConnectionKind, value)
	token.exchange = func(form url.Values, nonce string) (*openai.TokenResponse, *openai.SIWCIdentity, error) {
		require.Equal(t, "refresh_token", form.Get("grant_type"))
		require.Equal(t, "refresh-grant", form.Get("refresh_token"))
		require.Empty(t, nonce)
		return &openai.TokenResponse{AccessToken: "second-grant", RefreshToken: "refresh-new", ExpiresIn: 3600}, nil, nil
	}
	newValue, err := service.openAIConnectionToken(ctx, account.ID, siwcConnectionKind)
	require.NoError(t, err)
	require.Equal(t, "second-grant", newValue)
	service.invalidateOpenAIConnectionToken(ctx, account.ID, siwcConnectionKind, value)
	require.NoError(t, service.checkOpenAIConnectionEgress(ctx, account.ID, siwcConnectionKind, newValue))
	require.Error(t, service.checkOpenAIConnectionEgress(ctx, account.ID, siwcConnectionKind, value))
	require.NoError(t, service.RemoveOpenAIConnection(ctx, account.ID, siwcConnectionKind))
	require.Error(t, service.checkOpenAIConnectionEgress(ctx, account.ID, siwcConnectionKind, newValue))
	require.Equal(t, "primary", account.GetCredential("access_token"))
}
