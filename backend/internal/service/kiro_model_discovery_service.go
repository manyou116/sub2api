package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

const (
	kiroListAvailableModelsEndpointTmpl = "https://q.%s.amazonaws.com/ListAvailableModels"
	kiroAvailableModelsOrigin           = "AI_EDITOR"
	kiroAvailableModelsMaxResults       = 50
	kiroAvailableModelsCacheTTL         = 30 * time.Minute
	kiroAvailableModelsHTTPTimeout      = 20 * time.Second
	kiroAvailableModelsMaxPages         = 10
)

// KiroModelDiscovery fetches account-visible Kiro models.
type KiroModelDiscovery interface {
	ListAvailableModels(ctx context.Context, account *Account) ([]KiroAvailableModel, error)
}

// KiroModelDiscoveryService fetches account-visible Kiro models from Kiro Q Service.
type KiroModelDiscoveryService struct {
	tokenProvider *KiroTokenProvider
	now           func() time.Time
	endpointBase  string
	validateIP    bool

	cache atomic.Value // *kiroAvailableModelsCache
	sf    singleflight.Group
}

type kiroAvailableModelsCache struct {
	items map[string]kiroAvailableModelsCacheEntry
}

type kiroAvailableModelsCacheEntry struct {
	models   []KiroAvailableModel
	loadedAt time.Time
}

// NewKiroModelDiscoveryService creates a Kiro model discovery service.
func NewKiroModelDiscoveryService(tokenProvider *KiroTokenProvider) *KiroModelDiscoveryService {
	return &KiroModelDiscoveryService{
		tokenProvider: tokenProvider,
		now:           time.Now,
		validateIP:    true,
	}
}

// ListAvailableModels returns models available to a Kiro account.
func (s *KiroModelDiscoveryService) ListAvailableModels(ctx context.Context, account *Account) ([]KiroAvailableModel, error) {
	if account == nil || !account.IsKiro() {
		return nil, fmt.Errorf("kiro models: account is nil or not a Kiro account")
	}

	cacheKey := kiroAvailableModelsCacheKey(account)
	if models, ok := s.loadCached(cacheKey); ok {
		return models, nil
	}

	result, err, _ := s.sf.Do(cacheKey, func() (any, error) {
		if models, ok := s.loadCached(cacheKey); ok {
			return models, nil
		}
		models, fetchErr := s.fetchAvailableModels(ctx, account)
		if fetchErr != nil {
			return nil, fetchErr
		}
		s.storeCached(cacheKey, models)
		return models, nil
	})
	if err != nil {
		return nil, err
	}
	models, ok := result.([]KiroAvailableModel)
	if !ok {
		return nil, fmt.Errorf("kiro models: unexpected cache result type")
	}
	return copyKiroAvailableModels(models), nil
}

func (s *KiroModelDiscoveryService) loadCached(key string) ([]KiroAvailableModel, bool) {
	cached, ok := s.cache.Load().(*kiroAvailableModelsCache)
	if !ok || cached == nil {
		return nil, false
	}
	entry, ok := cached.items[key]
	if !ok {
		return nil, false
	}
	if s.currentTime().Sub(entry.loadedAt) >= kiroAvailableModelsCacheTTL {
		return nil, false
	}
	return copyKiroAvailableModels(entry.models), true
}

func (s *KiroModelDiscoveryService) storeCached(key string, models []KiroAvailableModel) {
	items := map[string]kiroAvailableModelsCacheEntry{}
	if cached, ok := s.cache.Load().(*kiroAvailableModelsCache); ok && cached != nil {
		items = make(map[string]kiroAvailableModelsCacheEntry, len(cached.items)+1)
		for k, v := range cached.items {
			items[k] = v
		}
	}
	items[key] = kiroAvailableModelsCacheEntry{
		models:   copyKiroAvailableModels(models),
		loadedAt: s.currentTime(),
	}
	s.cache.Store(&kiroAvailableModelsCache{items: items})
}

func (s *KiroModelDiscoveryService) fetchAvailableModels(ctx context.Context, account *Account) ([]KiroAvailableModel, error) {
	accessToken := strings.TrimSpace(account.KiroAccessToken())
	if s.tokenProvider != nil {
		if token, err := s.tokenProvider.EnsureFreshToken(ctx, account); err == nil && strings.TrimSpace(token) != "" {
			accessToken = strings.TrimSpace(token)
		}
	}
	if accessToken == "" {
		return nil, fmt.Errorf("kiro models: access_token empty")
	}

	models, body, status, err := s.fetchAvailableModelsWithToken(ctx, account, accessToken)
	if err == nil {
		return models, nil
	}
	if s.tokenProvider == nil || !isKiroAuthError(status, body) {
		return nil, err
	}

	newToken, refreshedAccount, refreshErr := s.tokenProvider.ForceRefresh(ctx, account)
	if refreshErr != nil || strings.TrimSpace(newToken) == "" {
		if refreshErr != nil {
			return nil, fmt.Errorf("%w; force refresh failed: %v", err, refreshErr)
		}
		return nil, err
	}
	return s.fetchAvailableModelsWithFreshToken(ctx, refreshedAccount, strings.TrimSpace(newToken))
}

func (s *KiroModelDiscoveryService) fetchAvailableModelsWithFreshToken(ctx context.Context, account *Account, accessToken string) ([]KiroAvailableModel, error) {
	models, _, _, err := s.fetchAvailableModelsWithToken(ctx, account, accessToken)
	return models, err
}

func (s *KiroModelDiscoveryService) fetchAvailableModelsWithToken(ctx context.Context, account *Account, accessToken string) ([]KiroAvailableModel, []byte, int, error) {
	var all []KiroAvailableModel
	seen := make(map[string]struct{})
	nextToken := ""

	for page := 0; page < kiroAvailableModelsMaxPages; page++ {
		respData, body, status, err := s.fetchAvailableModelsPage(ctx, account, accessToken, nextToken)
		if err != nil {
			return nil, body, status, err
		}
		for _, model := range respData.models() {
			converted := model.toAvailableModel()
			if converted.ID == "" {
				continue
			}
			if _, ok := seen[converted.ID]; ok {
				continue
			}
			seen[converted.ID] = struct{}{}
			all = append(all, converted)
		}
		nextToken = strings.TrimSpace(respData.NextToken)
		if nextToken == "" {
			break
		}
	}

	if len(all) == 0 {
		return nil, nil, http.StatusOK, fmt.Errorf("kiro models: empty model list")
	}
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].ID < all[j].ID
	})
	return all, nil, http.StatusOK, nil
}

func (s *KiroModelDiscoveryService) fetchAvailableModelsPage(ctx context.Context, account *Account, accessToken string, nextToken string) (*kiroListAvailableModelsResponse, []byte, int, error) {
	endpoint, err := s.buildListAvailableModelsURL(account, nextToken)
	if err != nil {
		return nil, nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("kiro models: build request: %w", err)
	}
	s.applyListAvailableModelsHeaders(req, account, accessToken)

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	client, err := httpclient.GetClient(httpclient.Options{
		ProxyURL:           strings.TrimSpace(proxyURL),
		Timeout:            kiroAvailableModelsHTTPTimeout,
		ValidateResolvedIP: s.validateIP,
	})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("kiro models: build http client: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("kiro models: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, resp.StatusCode, fmt.Errorf("kiro models: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, body, resp.StatusCode, fmt.Errorf("kiro models: HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}

	var data kiroListAvailableModelsResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, body, resp.StatusCode, fmt.Errorf("kiro models: parse response: %w", err)
	}
	return &data, body, resp.StatusCode, nil
}

func (s *KiroModelDiscoveryService) buildListAvailableModelsURL(account *Account, nextToken string) (string, error) {
	base := strings.TrimSpace(s.endpointBase)
	if base == "" {
		region := account.KiroRegion()
		if region == "" {
			region = KiroDefaultRegion
		}
		base = fmt.Sprintf(kiroListAvailableModelsEndpointTmpl, region)
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("kiro models: invalid endpoint: %w", err)
	}
	q := u.Query()
	q.Set("origin", kiroAvailableModelsOrigin)
	q.Set("maxResults", fmt.Sprintf("%d", kiroAvailableModelsMaxResults))
	if profileArn := strings.TrimSpace(account.KiroProfileArn()); profileArn != "" {
		q.Set("profileArn", profileArn)
	}
	if provider := strings.TrimSpace(account.GetCredential("model_provider")); provider != "" {
		q.Set("modelProvider", provider)
	}
	if nextToken != "" {
		q.Set("nextToken", nextToken)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *KiroModelDiscoveryService) applyListAvailableModelsHeaders(req *http.Request, account *Account, accessToken string) {
	machineID := account.KiroMachineID()
	if machineID == "" {
		machineID = "sub2api"
	}
	ua := fmt.Sprintf(KiroIDEUserAgentTmpl, machineID)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Amz-User-Agent", ua)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	req.Header.Set("amz-sdk-request", "attempt=1; max=1")
	if strings.EqualFold(account.KiroProvider(), "Internal") {
		req.Header.Set("redirect-for-internal", "true")
	}
}

func (s *KiroModelDiscoveryService) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func kiroAvailableModelsCacheKey(account *Account) string {
	region := account.KiroRegion()
	if region == "" {
		region = KiroDefaultRegion
	}
	return fmt.Sprintf("%d|%s|%s|%s|%s|%d", account.ID, region, account.KiroProvider(), account.KiroProfileArn(), account.GetCredential("model_provider"), account.GetCredentialAsInt64("expires_at"))
}

type kiroListAvailableModelsResponse struct {
	Models          []kiroDiscoveredModel `json:"models"`
	AvailableModels []kiroDiscoveredModel `json:"availableModels"`
	NextToken       string                `json:"nextToken"`
	DefaultModel    json.RawMessage       `json:"defaultModel"`
}

func (r *kiroListAvailableModelsResponse) models() []kiroDiscoveredModel {
	if r == nil {
		return nil
	}
	if len(r.Models) > 0 {
		return r.Models
	}
	return r.AvailableModels
}

type kiroDiscoveredModel struct {
	ModelID   string `json:"modelId"`
	ModelName string `json:"modelName"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
}

func (m kiroDiscoveredModel) toAvailableModel() KiroAvailableModel {
	id := strings.TrimSpace(m.ModelID)
	if id == "" {
		id = strings.TrimSpace(m.Name)
	}
	displayName := strings.TrimSpace(m.ModelName)
	if displayName == "" {
		displayName = id
	}
	return KiroAvailableModel{
		ID:          id,
		Type:        "model",
		DisplayName: displayName,
	}
}

func copyKiroAvailableModels(in []KiroAvailableModel) []KiroAvailableModel {
	if len(in) == 0 {
		return nil
	}
	out := make([]KiroAvailableModel, len(in))
	copy(out, in)
	return out
}
