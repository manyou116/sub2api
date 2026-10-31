package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const OpenAIOAuth401Key = "openai_oauth_401"
const OpenAIOAuth401Reason = "OAuth 401: expired access token"

// A fingerprint prevents reuse of the rejected AT; count bounds repeated
// rotations within ten minutes. Refreshing itself uses the ordinary expiry path.
type OpenAIOAuth401 struct {
	TokenHash string `json:"token_hash"`
	Count     int    `json:"count"`
	FirstAt   int64  `json:"first_at"`
}

type OpenAIOAuthExpiryRepository interface {
	ExpireOpenAIOAuthToken(context.Context, *Account, string, *OpenAIOAuth401, time.Time) (bool, error)
	ClearOpenAIOAuth401Pause(context.Context, int64, string) (bool, error)
}

func OpenAIOAuthTokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

func openAIOAuth401(a *Account) *OpenAIOAuth401 {
	if a == nil || a.Platform != PlatformOpenAI || a.Type != AccountTypeOAuth || a.Extra[OpenAIOAuth401Key] == nil {
		return nil
	}
	encoded, err := json.Marshal(a.Extra[OpenAIOAuth401Key])
	var marker OpenAIOAuth401
	if err != nil || json.Unmarshal(encoded, &marker) != nil || marker.TokenHash == "" {
		return nil
	}
	return &marker
}

func openAIOAuthTokenRejected(a *Account) bool {
	return a != nil && openAIOAuthTokenMatches401(a, a.GetOpenAIAccessToken())
}

func openAIOAuthTokenMatches401(a *Account, token string) bool {
	marker := openAIOAuth401(a)
	return marker != nil && marker.TokenHash == OpenAIOAuthTokenHash(token)
}

type openAI401HandledKey struct{}

func openAI401ResponseContext(ctx context.Context, response *http.Response) context.Context {
	if response != nil && response.Request != nil {
		if handled, _ := response.Request.Context().Value(openAI401HandledKey{}).(bool); handled {
			return context.WithValue(ctx, openAI401HandledKey{}, true)
		}
	}
	return ctx
}

func (s *OpenAIGatewayService) doOpenAIWithOAuthExpiry(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	response, err := s.doOpenAIOAuthCompressed(request, proxyURL, account)
	if err != nil || response == nil || response.StatusCode != http.StatusUnauthorized || s.cfg == nil || !s.cfg.TokenRefresh.Enabled || !s.cfg.TokenRefresh.OpenAI401RecoveryEnabled ||
		account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || request.Method != http.MethodPost || request.URL == nil || request.URL.Scheme != "https" || request.URL.Host != "chatgpt.com" || (request.Host != "" && request.Host != "chatgpt.com") ||
		(request.URL.Path != "/backend-api/codex/responses" && request.URL.Path != "/backend-api/codex/responses/compact") {
		return response, err
	}
	if response.Body == nil {
		response.Body = http.NoBody
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body = &openAIOAuthReplayResponseBody{Reader: io.MultiReader(bytes.NewReader(body), response.Body), Closer: response.Body}
	code := extractUpstreamErrorCode(body)
	if code != "token_expired" && (code != "" || !strings.Contains(strings.ToLower(extractUpstreamErrorMessage(body)), "your authentication token has expired")) {
		return response, nil
	}
	repo, ok := s.accountRepo.(OpenAIOAuthExpiryRepository)
	if !ok {
		return response, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), 5*time.Second)
	defer cancel()
	owner, readErr := resolveCredentialAccount(ctx, s.accountRepo, account)
	if readErr != nil || owner == nil {
		return response, nil
	}
	fresh, readErr := s.accountRepo.GetByID(ctx, owner.ID)
	if readErr != nil || fresh == nil || fresh.IsOpenAIPersonalAccessToken() || fresh.IsOpenAIAgentIdentity() || strings.TrimSpace(fresh.GetOpenAIRefreshToken()) == "" {
		return response, nil
	}
	token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	if token == "" || token == request.Header.Get("Authorization") {
		return response, nil
	}
	// A late response must not pause a newer AT, including a cached replacement.
	if fresh.GetOpenAIAccessToken() == token && !openAIOAuthTokenRejected(fresh) {
		now := time.Now()
		next := OpenAIOAuth401{TokenHash: OpenAIOAuthTokenHash(token), Count: 1, FirstAt: now.UnixMilli()}
		if previous := openAIOAuth401(fresh); previous != nil && previous.FirstAt <= now.UnixMilli() && now.UnixMilli()-previous.FirstAt < int64((10*time.Minute)/time.Millisecond) {
			next.Count, next.FirstAt = previous.Count+1, previous.FirstAt
		}
		applied, markErr := repo.ExpireOpenAIOAuthToken(ctx, fresh, token, &next, now.Add(10*time.Minute))
		if markErr != nil {
			slog.Warn("openai_oauth_expiry_mark_failed", "account_id", fresh.ID)
			return response, nil // Keep existing error handling if storage failed.
		}
		if applied {
			if s.openAITokenProvider != nil {
				if s.openAITokenProvider.tokenCache != nil {
					_ = s.openAITokenProvider.tokenCache.DeleteAccessToken(ctx, OpenAITokenCacheKey(fresh))
				}
				s.openAITokenProvider.refreshAPI.requestTokenRefresh(fresh.ID)
			}
			slog.Info("openai_oauth_expiry_marked", "account_id", fresh.ID, "attempt", next.Count)
		}
	}
	// Existing protocol/error logging still runs, without a second account mutation.
	if response.Request == nil {
		response.Request = &http.Request{}
	}
	response.Request = response.Request.WithContext(context.WithValue(request.Context(), openAI401HandledKey{}, true))
	return response, nil
}

func (api *OAuthRefreshAPI) requestTokenRefresh(id int64) {
	if api == nil {
		return
	}
	select {
	case api.refreshRequests <- id:
	default: // The ordinary durable expiry scan compensates for a full queue.
	}
}

func (s *TokenRefreshService) refreshRequestChannel() <-chan int64 {
	if s.refreshAPI == nil {
		return nil
	}
	return s.refreshAPI.refreshRequests
}

func (s *TokenRefreshService) refreshRequestedAccount(parent context.Context, id int64) {
	ctx, cancel := context.WithTimeout(parent, s.cycleTimeout())
	defer cancel()
	a, err := s.accountRepo.GetByID(ctx, id)
	if err != nil || a == nil || !a.IsActive() || a.Platform != PlatformOpenAI ||
		(a.TempUnschedulableUntil != nil && a.TempUnschedulableUntil.After(time.Now()) && strings.HasPrefix(a.TempUnschedulableReason, "token refresh retry exhausted:")) {
		return
	}
	for _, registration := range s.registrations {
		if registration.platform == PlatformOpenAI {
			state := &tokenRefreshProviderState{service: s, registration: registration, rateGate: s.providerRateGate(PlatformOpenAI), poolGate: s.providerConcurrencyGate(PlatformOpenAI)}
			s.processCandidatePage(ctx, []Account{*a}, map[string]*tokenRefreshProviderState{PlatformOpenAI: state}, time.Duration(s.cfg.RefreshBeforeExpiryHours*float64(time.Hour)))
			return
		}
	}
}

// Return true for our marker so ordinary cleanup cannot clear unrelated pauses.
func (s *TokenRefreshService) clearOpenAI401Pause(ctx context.Context, a *Account) bool {
	if a == nil || a.Platform != PlatformOpenAI || a.Type != AccountTypeOAuth ||
		((s.cfg == nil || !s.cfg.OpenAI401RecoveryEnabled) && openAIOAuth401(a) == nil) {
		return false
	}
	// A refresh may have started before the 401 was recorded. Reconcile the
	// durable row before cleanup/cache publication, retaining any admin changes.
	fresh, readErr := s.accountRepo.GetByID(ctx, a.ID)
	if readErr != nil || fresh == nil {
		return true
	}
	*a = *fresh
	if openAIOAuth401(a) == nil {
		return false
	}
	repo, ok := s.accountRepo.(OpenAIOAuthExpiryRepository)
	if !ok {
		return true
	}
	applied, err := repo.ClearOpenAIOAuth401Pause(ctx, a.ID, a.GetOpenAIAccessToken())
	if err == nil && applied {
		a.TempUnschedulableUntil, a.TempUnschedulableReason = nil, ""
		s.notifyAccountSchedulingBlockCleared(a.ID)
		if s.tempUnschedCache != nil {
			_ = s.tempUnschedCache.DeleteTempUnsched(ctx, a.ID)
		}
	}
	return true
}
