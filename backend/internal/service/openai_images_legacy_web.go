package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (s *OpenAIGatewayService) SetOpenAIWebImagesService(svc *OpenAIWebImagesService) {
	if s == nil {
		return
	}
	s.webImages = svc
}

// isAccountSchedulableForOpenAIRequest gates selection for chat vs web-image.
// Design:
//   - Text/Codex rate-limit (RateLimitResetAt / OverloadUntil / memory 429 fuse) is independent
//     from ChatGPT Web image quota.
//   - When Web images is enabled on an OAuth account, image requests may still schedule the
//     account even if text is rate-limited; only web cooldown / remaining=0 blocks images.
func (s *OpenAIGatewayService) isAccountSchedulableForOpenAIRequest(ctx context.Context, account *Account, imageCap OpenAIImagesCapability) bool {
	if account == nil {
		return false
	}
	if s.shouldBypassTextRateLimitForWebImages(account, imageCap, "") {
		if !account.IsSchedulableIgnoringTextRateLimit() {
			return false
		}
		if s.webImages != nil && s.webImages.IsWebRateLimited(ctx, account.ID) {
			return false
		}
		return true
	}
	return account.IsSchedulable()
}

// shouldBypassTextRateLimitForWebImages reports whether text/Codex 429 windows should be ignored
// for this account on an image request.
func (s *OpenAIGatewayService) shouldBypassTextRateLimitForWebImages(account *Account, imageCap OpenAIImagesCapability, requestedModel string) bool {
	if s == nil || account == nil || s.webImages == nil {
		return false
	}
	if !s.webImages.ShouldUseWebPath(account) {
		return false
	}
	if imageCap != "" {
		return true
	}
	return isOpenAIImageGenerationModel(requestedModel)
}

// isOpenAIAccountRuntimeBlockedForRequest skips the text/Codex in-memory 429 fuse for web-image path.
func (s *OpenAIGatewayService) isOpenAIAccountRuntimeBlockedForRequest(account *Account, imageCap OpenAIImagesCapability, requestedModel string) bool {
	if s.shouldBypassTextRateLimitForWebImages(account, imageCap, requestedModel) {
		return false
	}
	return s.isOpenAIAccountRuntimeBlocked(account)
}

func (s *OpenAIGatewayService) shouldUseOpenAILegacyWebImages(account *Account) bool {
	if s == nil || s.webImages == nil {
		return false
	}
	return s.webImages.ShouldUseWebPath(account)
}

func (s *OpenAIGatewayService) forwardOpenAIImagesLegacyWeb(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	parsed *OpenAIImagesRequest,
	channelMappedModel string,
) (*OpenAIForwardResult, error) {
	if s.webImages == nil {
		return nil, fmt.Errorf("web images service not configured")
	}
	if parsed == nil {
		return nil, fmt.Errorf("parsed images request is required")
	}
	start := time.Now()
	requestID := uuid.NewString()
	cfg := s.webImages.ParseAccountConfig(account)

	if s.webImages.cfgOrDefault().ProbeOnSchedule {
		if _, known := s.webImages.getQuotaCache(ctx, account.ID); !known {
			_, _ = s.webImages.ProbeAccount(ctx, account.ID, true)
		}
	}
	st, _ := s.webImages.GetStatus(ctx, account)
	if st != nil && !st.Schedulable {
		return nil, &UpstreamFailoverError{
			StatusCode:             http.StatusTooManyRequests,
			ResponseBody:           []byte(`{"error":{"message":"web image account not schedulable: ` + st.UnschedulableReason + `"}}`),
			RetryableOnSameAccount: false,
		}
	}

	ok, err := s.webImages.Acquire(ctx, account.ID, cfg.MaxInflight, requestID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &UpstreamFailoverError{
			StatusCode:             http.StatusTooManyRequests,
			ResponseBody:           []byte(`{"error":{"message":"web image account inflight full"}}`),
			RetryableOnSameAccount: false,
		}
	}
	defer s.webImages.Release(context.Background(), account.ID, requestID)

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		s.webImages.MarkFail(ctx, account, err.Error(), false)
		return nil, err
	}
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	sel := s.webImages.ResolveUpstream(account)
	// Business model (gpt-image-2) stays for billing/logging; upstream uses resolved ChatGPT slug.
	businessModel := strings.TrimSpace(channelMappedModel)
	if businessModel == "" {
		businessModel = strings.TrimSpace(parsed.Model)
	}
	if businessModel == "" {
		businessModel = "gpt-image-2"
	}
	genReq := webdriver.GenerateRequest{
		Prompt: parsed.Prompt, Model: sel.UpstreamModel, ThinkingEffort: sel.ThinkingEffort,
		N: parsed.N, Size: parsed.Size, Quality: parsed.Quality, ResponseFormat: parsed.ResponseFormat,
	}
	for _, up := range parsed.Uploads {
		genReq.Images = append(genReq.Images, webdriver.InputImage{FileName: up.FileName, ContentType: up.ContentType, Data: up.Data})
	}
	if parsed.MaskUpload != nil {
		genReq.Mask = &webdriver.InputImage{FileName: parsed.MaskUpload.FileName, ContentType: parsed.MaskUpload.ContentType, Data: parsed.MaskUpload.Data}
	}

	var result *webdriver.GenerateResult
	var lastErr error
	retries := s.webImages.cfgOrDefault().TransportMaxRetries
	if retries < 0 {
		retries = 0
	}
	logger.LegacyPrintf("service.openai_web_images", "web image resolve account=%d plan=%s mode=%s upstream=%s effort=%s source=%s", account.ID, sel.PlanType, sel.ModelMode, sel.UpstreamModel, sel.ThinkingEffort, sel.Source)
	for attempt := 0; attempt <= retries; attempt++ {
		if parsed.IsEdits() {
			result, lastErr = s.webImages.Driver().Edit(ctx, webdriver.Auth{AccessToken: token, ProxyURL: proxyURL}, genReq)
		} else {
			result, lastErr = s.webImages.Driver().Generate(ctx, webdriver.Auth{AccessToken: token, ProxyURL: proxyURL}, genReq)
		}
		if lastErr == nil {
			break
		}
		var we *webdriver.Error
		if asWebErr(lastErr, &we) && we.Kind == webdriver.ErrorKindTransport && attempt < retries {
			logger.LegacyPrintf("service.openai_web_images", "transport retry account=%d attempt=%d err=%s", account.ID, attempt+1, we.Message)
			time.Sleep(time.Duration(attempt+1) * 800 * time.Millisecond)
			continue
		}
		break
	}
	if lastErr != nil {
		rateLimited := false
		status := http.StatusBadGateway
		msg := lastErr.Error()
		var we *webdriver.Error
		if asWebErr(lastErr, &we) {
			msg = we.Message
			status = we.StatusCode
			if status == 0 {
				status = http.StatusBadGateway
			}
			switch we.Kind {
			case webdriver.ErrorKindRateLimited:
				// Only Free-plan / image-quota text should arm web cooldown + remaining=0.
				// Conversation GET HTTP 429 is transport throttle and must not burn the account.
				rateLimited = webdriver.IsImageQuotaLimitedMessage(msg)
			case webdriver.ErrorKindPolicy:
				s.webImages.MarkFail(ctx, account, msg, false)
				writeOpenAIWebImagesJSONError(c, status, "invalid_request_error", msg)
				return nil, &OpenAIImagesUpstreamError{StatusCode: status, ErrorType: "invalid_request_error", Message: msg}
			}
			if we.Retryable && we.Kind != webdriver.ErrorKindPolicy {
				s.webImages.MarkFail(ctx, account, msg, rateLimited)
				return nil, &UpstreamFailoverError{
					StatusCode: status, ResponseBody: []byte(fmt.Sprintf(`{"error":{"message":%q}}`, msg)), RetryableOnSameAccount: false,
				}
			}
		}
		s.webImages.MarkFail(ctx, account, msg, rateLimited)
		writeOpenAIWebImagesJSONError(c, status, "upstream_error", msg)
		return nil, &OpenAIImagesUpstreamError{StatusCode: status, ErrorType: "upstream_error", Message: msg}
	}

	s.webImages.MarkSuccess(ctx, account)
	body, err := buildOpenAIWebImagesResponse(result, parsed.ResponseFormat)
	if err != nil {
		return nil, err
	}
	c.Data(http.StatusOK, "application/json", body)
	imageCount := len(result.Data)
	if imageCount <= 0 {
		imageCount = parsed.N
	}
	return &OpenAIForwardResult{
		RequestID: requestID, Model: businessModel, UpstreamModel: sel.UpstreamModel, Stream: false, Duration: time.Since(start),
		ImageCount: imageCount, ImageSize: parsed.SizeTier, ImageInputSize: parsed.Size,
	}, nil
}

func asWebErr(err error, target **webdriver.Error) bool {
	if err == nil {
		return false
	}
	if we, ok := err.(*webdriver.Error); ok {
		*target = we
		return true
	}
	return false
}

func buildOpenAIWebImagesResponse(result *webdriver.GenerateResult, responseFormat string) ([]byte, error) {
	if result == nil {
		return nil, fmt.Errorf("empty result")
	}
	type item struct {
		B64JSON       string `json:"b64_json,omitempty"`
		URL           string `json:"url,omitempty"`
		RevisedPrompt string `json:"revised_prompt,omitempty"`
	}
	payload := map[string]any{"created": result.Created, "data": []item{}}
	format := strings.ToLower(strings.TrimSpace(responseFormat))
	if format == "" {
		format = "b64_json"
	}
	items := make([]item, 0, len(result.Data))
	for _, d := range result.Data {
		it := item{RevisedPrompt: d.RevisedPrompt}
		if format == "url" {
			it.URL = d.URL
			if it.URL == "" && d.B64JSON != "" {
				it.URL = "data:image/png;base64," + d.B64JSON
			}
		} else {
			it.B64JSON = d.B64JSON
		}
		items = append(items, it)
	}
	payload["data"] = items
	return json.Marshal(payload)
}

func writeOpenAIWebImagesJSONError(c *gin.Context, status int, errType, message string) {
	if c == nil || c.Writer.Written() {
		return
	}
	if status <= 0 {
		status = http.StatusBadGateway
	}
	c.JSON(status, gin.H{"error": gin.H{"message": message, "type": errType}})
}
