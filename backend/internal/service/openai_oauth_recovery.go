package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"
)

const OpenAIOAuthRecoveryKey = "openai_oauth_recovery"

// OpenAIOAuthRecovery is durable account-local state, never a credential store.
// Generation fences late failures/successes; TokenHash identifies the AT sent.
type OpenAIOAuthRecovery struct {
	Phase      string `json:"phase"`
	TokenHash  string `json:"token_hash"`
	Generation string `json:"generation"`
	Attempts   int    `json:"attempts"`
	RetryAt    int64  `json:"retry_at,omitempty"`
	LeaseUntil int64  `json:"lease_until,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// OpenAIOAuthRecoveryRepository atomically compares credentials, proxy and
// recovery state, updates only owned fields, and publishes scheduler invalidation.
type OpenAIOAuthRecoveryRepository interface {
	TransitionOpenAIOAuthRecovery(context.Context, *Account, *OpenAIOAuthRecovery, *OpenAIOAuthRecovery, map[string]any) (bool, error)
	ListOpenAIOAuthRecoveryCandidates(context.Context, int64, int) ([]Account, error)
}

func openAIRecoveryAccount(a *Account) bool {
	return a != nil && a.Platform == PlatformOpenAI && a.Type == AccountTypeOAuth && !a.IsCredentialShadow() && !a.IsOpenAIPersonalAccessToken() && !a.IsOpenAIAgentIdentity()
}

func openAIRecoveryTokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func openAIRecoveryState(a *Account) *OpenAIOAuthRecovery {
	if !openAIRecoveryAccount(a) || a.Extra == nil || a.Extra[OpenAIOAuthRecoveryKey] == nil {
		return nil
	}
	b, err := json.Marshal(a.Extra[OpenAIOAuthRecoveryKey])
	var state OpenAIOAuthRecovery
	if err != nil || json.Unmarshal(b, &state) != nil || state.Generation == "" {
		return nil
	}
	return &state
}

func openAIRecoveryMatches(a *Account, state *OpenAIOAuthRecovery) bool {
	return state != nil && (a.GetOpenAIAccessToken() == "" || state.TokenHash == openAIRecoveryTokenHash(a.GetOpenAIAccessToken()))
}

func openAIRecoveryBlocks(a *Account) bool {
	state := openAIRecoveryState(a)
	return openAIRecoveryMatches(a, state) && (state.Phase != "verify" || state.RetryAt > time.Now().UnixMilli())
}

var errOpenAIOAuthRecovering = errors.New("OpenAI OAuth credential recovery is pending")

type openAIRecoveryRejectedKey struct {
	AccountID int64
	TokenHash string
}

type openAIRecoveryStrictKey struct{}
type openAIRecoveryHandledKey struct{}

func (api *OAuthRefreshAPI) wakeOpenAIRecovery() {
	if api == nil || api.openAIRecoveryWake == nil {
		return
	}
	select {
	case api.openAIRecoveryWake <- struct{}{}:
	default:
	}
}

// finishOpenAIRecoveryRefresh runs before observing cancellation: a successful
// provider rotation must not be discarded just because its caller timed out.
func (api *OAuthRefreshAPI) finishOpenAIRecoveryRefresh(parent context.Context, a *Account, credentials map[string]any) (*OAuthRefreshResult, error) {
	repo, ok := api.accountRepo.(OpenAIOAuthRecoveryRepository)
	state := openAIRecoveryState(a)
	if !ok || state == nil || credentials == nil || credentials["access_token"] == "" {
		return nil, errOAuthRefreshCredentialPersist
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	credentials["_token_version"] = time.Now().UnixMilli()
	original := snapshotOAuthRefreshAccount(a)
	patch := make(map[string]any)
	for key, value := range credentials {
		if !reflect.DeepEqual(original.Credentials[key], value) {
			patch[key] = value
		}
	}
	for range 3 {
		next := *state
		next.Phase, next.LeaseUntil, next.RetryAt, next.Reason = "verify", 0, 0, "refreshed"
		token, _ := credentials["access_token"].(string)
		if token == "" {
			return nil, errOAuthRefreshCredentialPersist
		}
		if token == original.GetOpenAIAccessToken() {
			next.Phase, next.Reason = "attention", "no_replacement_at"
		}
		next.TokenHash = openAIRecoveryTokenHash(token)
		merged := MergeCredentials(a.Credentials, patch)
		applied, err := repo.TransitionOpenAIOAuthRecovery(ctx, a, state, &next, merged)
		if err == nil && applied {
			fresh, readErr := api.accountRepo.GetByID(ctx, a.ID)
			if api.tokenCache != nil {
				_ = api.tokenCache.DeleteAccessToken(ctx, OpenAITokenCacheKey(a))
			}
			if readErr != nil {
				return nil, readErr
			}
			return &OAuthRefreshResult{Refreshed: true, NewCredentials: merged, Account: fresh}, nil
		}
		// Retry persistence, never re-exchange a potentially consumed refresh token.
		fresh, readErr := api.accountRepo.GetByID(ctx, a.ID)
		if readErr != nil {
			return nil, errOAuthRefreshCredentialPersist
		}
		if fresh.GetOpenAIAccessToken() == token {
			return &OAuthRefreshResult{Refreshed: true, Account: fresh}, nil
		}
		current := openAIRecoveryState(fresh)
		if fresh.GetOpenAIRefreshToken() != original.GetOpenAIRefreshToken() || fresh.GetOpenAIAccessToken() != original.GetOpenAIAccessToken() || current == nil || current.Generation != state.Generation || !sameOptionalInt64(fresh.ProxyID, original.ProxyID) || !sameOpenAIRecoveryIdentity(fresh, original) {
			return &OAuthRefreshResult{Account: fresh}, nil
		}
		a, state = fresh, current
	}
	return nil, errOAuthRefreshCredentialPersist
}

func sameOptionalInt64(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
