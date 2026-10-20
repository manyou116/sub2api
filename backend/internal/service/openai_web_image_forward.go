package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
	"github.com/gin-gonic/gin"
)

// generate holds no slot of its own: public handlers and the admin test each
// acquire one ordinary account slot before invoking this adapter.
func (s *OpenAIWebImageService) generate(ctx context.Context, shadow *Account, parsed *OpenAIImagesRequest) (*webdriver.GenerateResult, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	fresh, err := s.accounts.GetByID(ctx, shadow.ID)
	if err != nil {
		return nil, "", err
	}
	if !fresh.IsWebImageShadow() {
		return nil, "", errors.New("web image shadow no longer exists")
	}
	parent, err := s.eligible(ctx, fresh)
	if err != nil {
		return nil, "", err
	}
	status, err := s.parentStatus(ctx, parent.ID)
	if err != nil {
		return nil, "", err
	}
	if status.ProbedAt == nil || (status.QuotaKnown && time.Since(*status.ProbedAt) > 30*time.Second) {
		if _, err = s.Probe(ctx, fresh.ID); errors.Is(err, errWebImageProbeBusy) {
			err = s.waitForQuotaProbe(ctx, parent.ID)
		}
		if err != nil {
			return nil, "", err
		}
		if _, err = s.eligible(ctx, fresh); err != nil {
			return nil, "", err
		}
	}
	auth, err := s.auth(ctx, parent)
	if err != nil {
		return nil, "", err
	}
	options := resolveWebImageOptions(fresh)
	request := webdriver.GenerateRequest{Prompt: parsed.Prompt, Model: options.Model, ThinkingEffort: options.ThinkingEffort, Size: parsed.Size, Quality: parsed.Quality, ResponseFormat: parsed.ResponseFormat, N: parsed.N}
	if options.RequireDownloadAttachment != nil && *options.RequireDownloadAttachment {
		request.Prompt += "\n请提供可下载文件/附件。"
	}
	for _, upload := range parsed.Uploads {
		request.Images = append(request.Images, webdriver.InputImage{FileName: upload.FileName, ContentType: upload.ContentType, Data: upload.Data})
	}
	if parsed.MaskUpload != nil {
		request.Mask = &webdriver.InputImage{FileName: parsed.MaskUpload.FileName, ContentType: parsed.MaskUpload.ContentType, Data: parsed.MaskUpload.Data}
	}
	fetch := func(raw string) (webdriver.InputImage, error) {
		encoded, err := s.gateway.fetchOpenAIImageURLBase64(ctx, parent, raw)
		if err != nil {
			return webdriver.InputImage{}, err
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return webdriver.InputImage{}, err
		}
		if len(data) > openAIImageMaxUploadPartSize || !isBackfillImageContent(data) {
			return webdriver.InputImage{}, errors.New("invalid or oversized input image")
		}
		return webdriver.InputImage{Data: data, ContentType: http.DetectContentType(data)}, nil
	}
	for _, url := range parsed.InputImageURLs {
		image, err := fetch(url)
		if err != nil {
			return nil, options.Model, webImageInputError(err)
		}
		request.Images = append(request.Images, image)
	}
	if parsed.MaskImageURL != "" {
		image, err := fetch(parsed.MaskImageURL)
		if err != nil {
			return nil, options.Model, webImageInputError(err)
		}
		request.Mask = &image
	}
	var result *webdriver.GenerateResult
	if parsed.IsEdits() {
		result, err = s.driver.Edit(ctx, auth, request)
	} else {
		result, err = s.driver.Generate(ctx, auth, request)
	}
	if err != nil {
		s.redactDriverError(err, parent)
		s.observeError(ctx, parent, options.Model, err)
		return result, options.Model, err
	}
	return result, options.Model, nil
}

func (s *OpenAIGatewayService) forwardWebImage(ctx context.Context, c *gin.Context, account *Account, parsed *OpenAIImagesRequest, channelMappedModel string) (*OpenAIForwardResult, error) {
	if s.webImages == nil {
		return nil, errors.New("web image service unavailable")
	}
	requestModel := strings.TrimSpace(channelMappedModel)
	if requestModel == "" {
		requestModel = parsed.Model
	}
	businessModel := account.GetMappedModel(requestModel)
	if !IsGPTImageGenerationModel(businessModel) {
		upstreamErr := webImageInputError(errors.New("web image shadow requires an image model"))
		writeOpenAIImagesUpstreamErrorResponse(c, upstreamErr)
		return nil, upstreamErr
	}
	SetOpsUpstreamModel(c, businessModel)
	started := time.Now()
	generated, _, err := s.webImages.generate(ctx, account, parsed)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		var upstreamErr *OpenAIImagesUpstreamError
		if errors.As(err, &upstreamErr) {
			writeOpenAIImagesUpstreamErrorResponse(c, upstreamErr)
			return nil, upstreamErr
		}
		status := http.StatusBadGateway
		var driverErr *webdriver.Error
		if errors.As(err, &driverErr) && driverErr.StatusCode >= 400 {
			status = driverErr.StatusCode
		}
		retryHeaders := http.Header{}
		if driverErr != nil && driverErr.RetryAfter > 0 {
			retryHeaders.Set("Retry-After", fmt.Sprint(max(1, int(driverErr.RetryAfter.Seconds()+0.999))))
		}
		if driverErr != nil && driverErr.MayHaveGenerated() {
			c.Header("Retry-After", retryHeaders.Get("Retry-After"))
			upstreamErr = webImageResultUnconfirmed(driverErr)
			// A definitive rejection in the stream is terminal too, but its known
			// policy/quota reason is more useful than an uncertain-result message.
			if driverErr.Kind == webdriver.ErrorKindPolicy || webImageQuotaFailure(driverErr) {
				status = http.StatusTooManyRequests
				if driverErr.Kind == webdriver.ErrorKindPolicy {
					status = http.StatusBadRequest
				}
				upstreamErr = &OpenAIImagesUpstreamError{StatusCode: status, ErrorType: "invalid_request_error", Code: driverErr.Code, Message: sanitizeWebImageError(driverErr.Message)}
			}
			writeOpenAIImagesUpstreamErrorResponse(c, upstreamErr)
			return nil, upstreamErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if driverErr != nil && !driverErr.Retryable && driverErr.Kind != webdriver.ErrorKindAuth {
			c.Header("Retry-After", retryHeaders.Get("Retry-After"))
			errorType := "upstream_error"
			if driverErr.Kind == webdriver.ErrorKindPolicy || (driverErr.Kind == webdriver.ErrorKindInternal && driverErr.Stage == "edit") {
				status = http.StatusBadRequest
			}
			if status >= 400 && status < 500 {
				errorType = "invalid_request_error"
			}
			upstreamErr = &OpenAIImagesUpstreamError{StatusCode: status, ErrorType: errorType, Code: driverErr.Code, Message: sanitizeWebImageError(driverErr.Message)}
			writeOpenAIImagesUpstreamErrorResponse(c, upstreamErr)
			return nil, upstreamErr
		}
		payload, _ := json.Marshal(gin.H{"error": gin.H{"message": sanitizeWebImageError(err.Error()), "type": "web_image_error"}})
		// Failover remains bounded by the ordinary Images handler. Never fall back
		// from this account to another protocol on the same identity.
		return nil, &UpstreamFailoverError{StatusCode: status, ResponseBody: payload, ResponseHeaders: retryHeaders}
	}
	if generated == nil || len(generated.Data) == 0 {
		return nil, errors.New("web image generation returned no images")
	}
	result := &OpenAIForwardResult{Model: requestModel, UpstreamModel: businessModel, UpstreamEndpoint: "/backend-api/conversation", Duration: time.Since(started), ImageCount: len(generated.Data), ImageSize: parsed.SizeTier, ImageInputSize: parsed.Size, Stream: parsed.Stream, Usage: OpenAIUsage{InputTokens: generated.Usage.InputTokens, OutputTokens: generated.Usage.OutputTokens, ImageOutputTokens: generated.Usage.ImageOutputTokens}}
	SetActualOpenAIUpstreamEndpoint(c, result.UpstreamEndpoint)
	data := make([]map[string]any, 0, len(generated.Data))
	for _, item := range generated.Data {
		entry := map[string]any{"revised_prompt": item.RevisedPrompt}
		if parsed.ResponseFormat == "url" && item.URL != "" {
			entry["url"] = item.URL
		} else {
			entry["b64_json"] = item.B64JSON
		}
		data = append(data, entry)
	}
	payload := map[string]any{"created": generated.Created, "data": data}
	if !parsed.Stream {
		c.JSON(http.StatusOK, payload)
		return result, nil
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	for i, item := range data {
		event := map[string]any{"type": "image_generation.completed", "created_at": generated.Created, "output_index": i}
		for key, value := range item {
			event[key] = value
		}
		encoded, encodeErr := json.Marshal(event)
		if encodeErr != nil {
			return result, encodeErr
		}
		if _, err = fmt.Fprintf(c.Writer, "event: image_generation.completed\ndata: %s\n\n", encoded); err != nil {
			return result, err
		}
		c.Writer.Flush()
	}
	_, err = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
	return result, err
}

func (s *OpenAIGatewayService) webImageSchedulingReason(ctx context.Context, account *Account, model string) string {
	if !account.IsWebImageShadow() {
		return ""
	}
	if strings.TrimSpace(model) == "" || !OpenAIImageGenerationIntentFromContext(ctx) || !IsGPTImageGenerationModel(account.GetMappedModel(model)) {
		return "web_image_only"
	}
	if s.webImages == nil {
		return "web_image_service_unavailable"
	}
	if _, err := s.webImages.eligible(ctx, account); err != nil {
		return webImageFailureReason(err)
	}
	return ""
}

func (s *AccountTestService) testWebImage(c *gin.Context, account *Account, model, prompt string) error {
	gateway := s.openaiGatewayService
	if gateway == nil || gateway.webImages == nil || gateway.concurrencyService == nil {
		return s.sendErrorAndEnd(c, "Web image service unavailable")
	}
	if !IsGPTImageGenerationModel(model) {
		return s.sendErrorAndEnd(c, "Web image shadows only support image tests")
	}
	if account.Concurrency <= 0 {
		return s.sendErrorAndEnd(c, "Web image concurrency must be positive")
	}
	slot, err := gateway.concurrencyService.AcquireAccountSlot(c.Request.Context(), account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	if !slot.Acquired {
		return s.sendErrorAndEnd(c, "Web image account is at capacity")
	}
	defer slot.ReleaseFunc()
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Writer.Flush()
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	if strings.TrimSpace(prompt) == "" {
		prompt = defaultOpenAIImageTestPrompt
	}
	parsed := &OpenAIImagesRequest{Endpoint: openAIImagesGenerationsEndpoint, Model: model, Prompt: prompt}
	applyOpenAIImagesDefaults(parsed)
	result, _, err := gateway.webImages.generate(c.Request.Context(), account, parsed)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	if result == nil || len(result.Data) == 0 {
		return s.sendErrorAndEnd(c, "No images returned")
	}
	for _, item := range result.Data {
		url := item.URL
		if item.B64JSON != "" {
			url = "data:image/png;base64," + item.B64JSON
		}
		s.sendEvent(c, TestEvent{Type: "image", ImageURL: url})
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *OpenAIGatewayService) openAIRequestPrivacySet(ctx context.Context, account *Account) bool {
	if account == nil {
		return false
	}
	if !account.IsWebImageShadow() {
		return account.IsPrivacySet()
	}
	if s.webImages == nil {
		return false
	}
	parent, err := s.webImages.parent(ctx, account)
	return err == nil && parent.IsPrivacySet()
}

func webImageInputError(err error) *OpenAIImagesUpstreamError {
	return &OpenAIImagesUpstreamError{StatusCode: http.StatusBadRequest, ErrorType: "invalid_request_error", Message: sanitizeWebImageError(err.Error())}
}
