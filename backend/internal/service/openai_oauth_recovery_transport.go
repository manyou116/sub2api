package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func openAIRecoveryResponseContext(ctx context.Context, response *http.Response) context.Context {
	if response != nil && response.Request != nil {
		if handled, ok := response.Request.Context().Value(openAIRecoveryHandledKey{}).(bool); ok {
			return context.WithValue(ctx, openAIRecoveryHandledKey{}, handled)
		}
	}
	return ctx
}

func (s *OpenAIGatewayService) doOpenAIWithOAuthRecovery(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	repo, ok := s.accountRepo.(OpenAIOAuthRecoveryRepository)
	if !ok || s.cfg == nil || !s.cfg.TokenRefresh.Enabled || !s.cfg.TokenRefresh.OpenAI401RecoveryEnabled || account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || request.URL == nil || request.URL.Scheme != "https" || request.URL.Host != "chatgpt.com" || (request.URL.Path != "/backend-api/codex/responses" && request.URL.Path != "/backend-api/codex/responses/compact") {
		return s.doOpenAIOAuthCompressed(request, proxyURL, account)
	}
	owner, err := resolveCredentialAccount(request.Context(), s.accountRepo, account)
	if err != nil {
		return nil, err
	}
	if !openAIRecoveryAccount(owner) {
		return s.doOpenAIOAuthCompressed(request, proxyURL, account)
	}
	// Final admission fences stale scheduler and token-cache reads.
	fresh, err := s.accountRepo.GetByID(request.Context(), owner.ID)
	if err != nil || fresh == nil {
		return nil, openAIRecoveryFailover()
	}
	if !sameOpenAIRecoveryIdentity(owner, fresh) || !sameOptionalInt64(fresh.ProxyID, owner.ProxyID) || (!account.IsShadow() && !fresh.IsSchedulable()) || (account.IsShadow() && !fresh.IsCredentialUsableForShadow()) {
		return nil, openAIRecoveryFailover()
	}
	if s.openAITokenProvider != nil && s.openAITokenProvider.refreshAPI != nil {
		key := openAIRecoveryRejectedKey{fresh.ID, openAIRecoveryTokenHash(fresh.GetOpenAIAccessToken())}
		if _, rejected := s.openAITokenProvider.refreshAPI.openAIRecoveryRejected.Load(key); rejected {
			return nil, openAIRecoveryFailover()
		}
	}
	state := openAIRecoveryState(fresh)
	if state != nil && openAIRecoveryMatches(fresh, state) && state.Phase == "verify" {
		next := *state
		next.Phase = "probing"
		next.LeaseUntil = time.Now().Add(time.Minute).UnixMilli()
		applied, transitionErr := repo.TransitionOpenAIOAuthRecovery(request.Context(), fresh, state, &next, nil)
		if transitionErr != nil || !applied {
			return nil, openAIRecoveryFailover()
		}
		state = &next
	}
	token := fresh.GetOpenAIAccessToken()
	if token == "" {
		return nil, openAIRecoveryFailover()
	}
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+token)
	var stopHeaderTimer func() bool
	var cancelProbe context.CancelFunc
	if state != nil && state.Phase == "probing" {
		probeCtx, cancel := context.WithCancel(request.Context())
		cancelProbe = cancel
		stopHeaderTimer = time.AfterFunc(30*time.Second, cancel).Stop
		request = request.WithContext(probeCtx)
	}
	response, sendErr := s.doOpenAIOAuthCompressed(request, proxyURL, account)
	if stopHeaderTimer != nil {
		stopHeaderTimer()
		if response != nil && response.Body != nil {
			response.Body = &openAIRequestContextReadCloser{ReadCloser: response.Body, cleanup: cancelProbe}
		} else {
			cancelProbe()
		}
	}
	s.observeOpenAIRecoveryResponse(request.Context(), repo, fresh, state, token, response, sendErr)
	return response, sendErr
}

func openAIRecoveryFailover() error {
	return &UpstreamFailoverError{StatusCode: http.StatusUnauthorized, ResponseBody: []byte(`{"error":{"type":"upstream_error","message":"OpenAI OAuth credential recovery is pending"}}`)}
}

func (s *OpenAIGatewayService) observeOpenAIRecoveryResponse(parent context.Context, repo OpenAIOAuthRecoveryRepository, owner *Account, probe *OpenAIOAuthRecovery, token string, response *http.Response, sendErr error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	is401 := response != nil && response.StatusCode == http.StatusUnauthorized
	var body []byte
	if is401 {
		// Preserve bytes for the existing protocol and ops handlers.
		if response.Body == nil {
			response.Body = http.NoBody
		}
		body, _ = io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body = &openAIRecoveryReplayBody{Reader: io.MultiReader(bytes.NewReader(body), response.Body), Closer: response.Body}
		if response.Request == nil {
			response.Request = &http.Request{}
		}
		response.Request = response.Request.WithContext(context.WithValue(parent, openAIRecoveryHandledKey{}, true))
	}
	if is401 && s.openAITokenProvider != nil && s.openAITokenProvider.refreshAPI != nil {
		s.openAITokenProvider.refreshAPI.openAIRecoveryRejected.LoadOrStore(openAIRecoveryRejectedKey{owner.ID, openAIRecoveryTokenHash(token)}, openAIRecoveryRejectedState(owner, probe, token, body))
		s.openAITokenProvider.refreshAPI.wakeOpenAIRecovery()
	}
	if !is401 && (probe == nil || probe.Phase != "probing") {
		return
	}
	fresh, err := s.accountRepo.GetByID(ctx, owner.ID)
	if err != nil || fresh == nil {
		return
	}
	current := openAIRecoveryState(fresh)
	if fresh.GetOpenAIAccessToken() != token {
		return
	}
	if !is401 {
		if current == nil || probe == nil || current.Generation != probe.Generation || current.Phase != "probing" {
			return
		}
		var next *OpenAIOAuthRecovery
		if sendErr != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
			v := *current
			v.Phase = "verify"
			v.LeaseUntil = 0
			v.RetryAt = time.Now().Add(30 * time.Second).UnixMilli()
			next = &v
		}
		_, _ = repo.TransitionOpenAIOAuthRecovery(ctx, fresh, current, next, nil)
		return
	}
	next := openAIRecoveryRejectedState(fresh, current, token, body)
	if current != nil && openAIRecoveryMatches(fresh, current) && current.Phase != "probing" && current.Phase != "verify" {
		return
	}
	applied, err := repo.TransitionOpenAIOAuthRecovery(ctx, fresh, current, next, nil)
	if err != nil || !applied {
		slog.Warn("openai_oauth_recovery_quarantine_failed", "account_id", fresh.ID)
		return
	}
	if s.openAITokenProvider != nil {
		if s.openAITokenProvider.tokenCache != nil {
			_ = s.openAITokenProvider.tokenCache.DeleteAccessToken(ctx, OpenAITokenCacheKey(fresh))
		}
		if next.Phase == "pending" {
			s.openAITokenProvider.refreshAPI.wakeOpenAIRecovery()
		}
	}
	slog.Info("openai_oauth_recovery_quarantined", "account_id", fresh.ID, "phase", next.Phase, "reason", next.Reason)
}

type openAIRecoveryReplayBody struct {
	io.Reader
	io.Closer
}

func sameOpenAIRecoveryIdentity(a, b *Account) bool {
	for _, key := range []string{"client_id", "chatgpt_account_id", "chatgpt_user_id"} {
		if a.GetCredential(key) != b.GetCredential(key) {
			return false
		}
	}
	return true
}

func openAIRecoveryRejectedState(a *Account, current *OpenAIOAuthRecovery, token string, body []byte) *OpenAIOAuthRecovery {
	code := strings.ToLower(extractUpstreamErrorCode(body))
	message := strings.ToLower(extractUpstreamErrorMessage(body))
	next := &OpenAIOAuthRecovery{Phase: "attention", TokenHash: openAIRecoveryTokenHash(token), Generation: rand.Text(), Reason: "unknown_401"}
	switch {
	case code == "token_revoked" || code == "token_invalidated" || code == "account_deactivated" || strings.TrimSpace(a.GetOpenAIRefreshToken()) == "":
		next.Phase = "reauth"
		next.Reason = "credential_invalid"
	case current != nil && openAIRecoveryMatches(a, current):
		next.Reason = "replacement_rejected"
	case code == "token_expired" || strings.Contains(message, "your authentication token has expired"):
		next.Phase = "pending"
		next.Reason = "token_expired"
	}
	return next
}
