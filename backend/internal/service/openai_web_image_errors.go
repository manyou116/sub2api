package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

type webImageUnavailable struct{ status *WebImageStatus }

func (e *webImageUnavailable) Error() string {
	if e.status.CooldownReason == "quota_exhausted" || (e.status.Remaining != nil && *e.status.Remaining <= 0) {
		return "web image quota is exhausted"
	}
	return "web image temporarily unavailable: " + e.status.CooldownReason
}

func webImageUnavailableError(status *WebImageStatus) error { return &webImageUnavailable{status} }

func (s *OpenAIWebImageService) observeError(ctx context.Context, parent *Account, model string, err error) {
	if ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var failure *webdriver.Error
	if !errors.As(err, &failure) {
		return
	}
	// Policy and request errors describe this request, never credential health.
	if failure.Kind == webdriver.ErrorKindPolicy {
		return
	}
	if failure.Kind == webdriver.ErrorKindAuth {
		if s.gateway.rateLimitService != nil {
			s.gateway.rateLimitService.HandleUpstreamError(ctx, parent, http.StatusUnauthorized, nil, failure.ResponseBody)
		}
		return
	}
	quotaExhausted := webImageQuotaFailure(failure) && !webdriver.IsWorkModeModel(model)
	if quotaExhausted {
		until := time.Now().Add(24 * time.Hour)
		if failure.ResetAt != nil && failure.ResetAt.After(time.Now()) {
			until = *failure.ResetAt
		}
		if failure.RetryAfter > 0 && failure.ResetAt == nil {
			until = time.Now().Add(min(failure.RetryAfter, 24*time.Hour))
		}
		if repo, ok := s.accounts.(WebImageStateRepository); ok {
			s.logStateError(parent.ID, "persist_quota", repo.SetWebImageCooldown(ctx, parent.ID, until))
		}
		if s.quotaCache != nil {
			zero := 0
			s.logStateError(parent.ID, "exhaust_quota", s.quotaCache.Exhaust(ctx, parent.ID, &WebImageStatus{ParentAccountID: parent.ID, QuotaKnown: true, Remaining: &zero, ResetAt: &until}))
		}
		return
	}
	reason := ""
	switch {
	case failure.Kind == webdriver.ErrorKindRateLimited:
		reason = "rate_limit"
	case webdriver.IsHTMLForbiddenResponse(failure.StatusCode, failure.ResponseBody):
		reason = "edge_block"
	case failure.Kind == webdriver.ErrorKindTransport:
		if classifyUpstreamTransportError(failure).Persistent {
			s.gateway.tempUnscheduleOpenAITransportError(ctx, parent, sanitizeWebImageError(failure.Message))
			return
		}
		reason = "network_error"
	}
	if reason == "" || s.quotaCache == nil {
		return
	}
	delay := min(failure.RetryAfter, 24*time.Hour)
	if delay < 30*time.Second {
		delay = 30 * time.Second
	}
	until := time.Now().Add(delay)
	if failure.ResetAt != nil && failure.ResetAt.After(until) {
		until = *failure.ResetAt
	}
	s.logStateError(parent.ID, "backoff", s.quotaCache.Backoff(ctx, parent.ID, reason, until))
}

func (s *OpenAIWebImageService) logStateError(parentID int64, operation string, err error) {
	if err != nil {
		slog.Warn("web_image_state_update_failed", "parent_account_id", parentID, "operation", operation, "error", err)
	}
}

func webImageRetrySeconds(until *time.Time) int {
	if until == nil {
		return 0
	}
	return max(0, int(time.Until(*until).Seconds()+0.999))
}

func webImageFailureReason(err error) string {
	var unavailable *webImageUnavailable
	if errors.As(err, &unavailable) {
		return "web_image_" + unavailable.status.CooldownReason
	}
	return "web_image_unavailable"
}

func webImageResultUnconfirmed(failure *webdriver.Error) *OpenAIImagesUpstreamError {
	return &OpenAIImagesUpstreamError{StatusCode: http.StatusBadGateway, ErrorType: "upstream_error", Code: "web_image_result_unconfirmed", Message: fmt.Sprintf("Generation may have been accepted; automatic regeneration was stopped after %s failed. %s", failure.Stage, sanitizeWebImageError(failure.Message))}
}

func webImageQuotaFailure(failure *webdriver.Error) bool {
	return failure.Kind == webdriver.ErrorKindRateLimited && webdriver.IsImageQuotaLimitedMessage(failure.Code+" "+failure.Message)
}

var webImageURLCredentials = regexp.MustCompile(`(?i)(https?|socks5h?)://[^/@\s]+@`)
var webImageBearer = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9_.~+/-]+`)

func sanitizeWebImageError(message string) string {
	message = webImageURLCredentials.ReplaceAllString(message, "$1://***@")
	message = webImageBearer.ReplaceAllString(message, "Bearer ***")
	return logredact.RedactText(sanitizeUpstreamErrorMessage(message), "token", "sig", "signature", "se", "sv")
}
func (s *OpenAIWebImageService) redactDriverError(err error, parent *Account) {
	var failure *webdriver.Error
	if !errors.As(err, &failure) {
		return
	}
	for _, value := range []string{parent.GetOpenAIAccessToken(), parent.GetCredential("refresh_token")} {
		if value != "" {
			failure.Message = strings.ReplaceAll(failure.Message, value, "***")
		}
	}
	if parent.Proxy != nil && parent.Proxy.Password != "" {
		failure.Message = strings.ReplaceAll(failure.Message, parent.Proxy.Password, "***")
	}
	failure.Message = sanitizeWebImageError(failure.Message)
}
