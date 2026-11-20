package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
)

// Work owns no quota snapshot: observations update the same parent resource that
// ordinary Codex requests use. Generic read throttles never assert exhaustion.
func (s *OpenAIWebImageService) observeImageShadowError(ctx context.Context, shadow, parent *Account, model string, err error) {
	if !shadow.IsWorkImageShadow() {
		s.observeError(ctx, parent, model, err)
		return
	}
	var failure *webdriver.Error
	if ctx.Err() != nil || !errors.As(err, &failure) {
		return
	}
	if failure.Kind == webdriver.ErrorKindAuth || failure.Kind == webdriver.ErrorKindTransport {
		// Reuse shared identity/transport handling. Short network backoff belongs
		// to this child, not to the independent Web image resource.
		if failure.Kind == webdriver.ErrorKindAuth || classifyUpstreamTransportError(failure).Persistent {
			s.observeError(ctx, parent, model, err)
			return
		}
	}
	if failure.Kind == webdriver.ErrorKindPolicy {
		return
	}
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	globalQuota, modelQuota := workImageQuotaFailure(failure)
	if failure.Kind == webdriver.ErrorKindRateLimited && (globalQuota || modelQuota) {
		until := time.Now().Add(30 * time.Second)
		if failure.RetryAfter > 0 {
			until = time.Now().Add(min(failure.RetryAfter, 24*time.Hour))
		}
		if failure.ResetAt != nil && failure.ResetAt.After(time.Now()) {
			until = *failure.ResetAt
		}
		if globalQuota && s.gateway.rateLimitService != nil {
			// Normalize only the known quota signal into the existing Codex handler.
			body, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "usage_limit_reached", "message": failure.Message, "resets_at": until.Unix()}})
			s.gateway.rateLimitService.HandleUpstreamError(stateCtx, parent, http.StatusTooManyRequests, nil, body, imageShadowCodexModel(model))
		} else if modelQuota {
			s.logStateError(parent.ID, "work_model_quota", s.accounts.SetModelRateLimit(stateCtx, parent.ID, imageShadowCodexModel(model), until, "work_image_model_quota"))
		}
		return
	}
	if failure.Kind == webdriver.ErrorKindRateLimited || failure.Kind == webdriver.ErrorKindTransport || webdriver.IsHTMLForbiddenResponse(failure.StatusCode, failure.ResponseBody) {
		delay := min(failure.RetryAfter, 24*time.Hour)
		if delay < 30*time.Second {
			delay = 30 * time.Second
		}
		until := time.Now().Add(delay)
		s.logStateError(shadow.ID, "work_transport_backoff", s.accounts.SetTempUnschedulable(stateCtx, shadow.ID, until, "work_image_temporary_backoff"))
	}
}

func workImageQuotaFailure(failure *webdriver.Error) (global, model bool) {
	if failure == nil || failure.Kind != webdriver.ErrorKindRateLimited {
		return false, false
	}
	code := strings.ToLower(failure.Code)
	return code == "usage_limit_reached" || code == "codex_usage_limit_reached" || code == "codex_quota_exhausted", code == "model_usage_limit_reached" || code == "model_quota_exhausted"
}
