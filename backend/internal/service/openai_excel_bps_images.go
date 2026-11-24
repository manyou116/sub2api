// Excel BPS Images protocol adapted from ranxi2001/sub2api (e90811e), LGPL-3.0.
package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func excelBPSImagesSupportedModel(model string) bool {
	return strings.TrimSpace(model) == "gpt-image-2"
}

// excelBPSImagesUnsupportedReason reports why a request must stay on Codex.
// BPS answers 422 for any field outside model, prompt, size, quality, n,
// background and output_format; only png output and opaque/auto backgrounds
// were accepted in testing.
func excelBPSImagesUnsupportedReason(parsed *OpenAIImagesRequest) string {
	switch {
	case parsed == nil:
		return "missing request"
	case parsed.Stream:
		return "stream"
	case parsed.PartialImages != nil:
		return "partial_images"
	case parsed.OutputCompression != nil:
		return "output_compression"
	case strings.TrimSpace(parsed.Style) != "":
		return "style"
	case strings.TrimSpace(parsed.InputFidelity) != "":
		return "input_fidelity"
	}
	switch strings.ToLower(strings.TrimSpace(parsed.OutputFormat)) {
	case "", "png":
	default:
		return "output_format"
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Moderation)) {
	case "", "auto":
	default:
		return "moderation"
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Background)) {
	case "", "opaque", "auto":
	default:
		return "background"
	}
	if parsed.IsEdits() {
		return excelBPSImagesEditsUnsupportedReason(parsed)
	}
	return ""
}

// The BPS edit endpoint requires exactly one image file; masks and remote
// image URLs (which the gateway would have to download) stay on Codex.
func excelBPSImagesEditsUnsupportedReason(parsed *OpenAIImagesRequest) string {
	if parsed.HasMask || parsed.MaskUpload != nil || strings.TrimSpace(parsed.MaskImageURL) != "" {
		return "mask"
	}
	images := len(parsed.Uploads)
	for _, imageURL := range parsed.InputImageURLs {
		if imageURL = strings.TrimSpace(imageURL); imageURL == "" {
			continue
		}
		images++
		if _, _, err := decodeAccountTestDataURL(imageURL); err != nil {
			return "remote image_url"
		}
	}
	if images != 1 {
		return "image count"
	}
	return ""
}

// buildExcelBPSImagesPayload reuses the Codex generations body. moderation=auto
// is the upstream default, and BPS rejects the field itself.
func buildExcelBPSImagesPayload(parsed *OpenAIImagesRequest, model string) ([]byte, error) {
	body, _, err := buildOpenAIImagesOAuthPayload(parsed, model)
	if err != nil {
		return nil, err
	}
	return sjson.DeleteBytes(body, "moderation")
}

var excelBPSImagesQuoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, `\"`)

// buildExcelBPSImagesEditsBody builds the multipart edit request. Unlike the
// Codex edit endpoint, which takes JSON data URLs, BPS wants one file part
// plus the same form fields the generations endpoint accepts.
func buildExcelBPSImagesEditsBody(parsed *OpenAIImagesRequest, model string) ([]byte, string, error) {
	prompt := parsed.Prompt
	if !parsed.Multipart && gjson.ValidBytes(parsed.Body) {
		if rawPrompt := gjson.GetBytes(parsed.Body, "prompt").String(); rawPrompt != "" {
			prompt = rawPrompt
		}
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, "", fmt.Errorf("prompt is required")
	}
	fileName, contentType, data := "", "", []byte(nil)
	if len(parsed.Uploads) > 0 {
		upload := parsed.Uploads[0]
		fileName, contentType, data = upload.FileName, upload.ContentType, upload.Data
	} else {
		for _, imageURL := range parsed.InputImageURLs {
			if imageURL = strings.TrimSpace(imageURL); imageURL == "" {
				continue
			}
			decoded, mimeType, err := decodeAccountTestDataURL(imageURL)
			if err != nil {
				return nil, "", err
			}
			contentType, data = mimeType, decoded
			break
		}
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("image input is required")
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = http.DetectContentType(data)
	}
	if strings.TrimSpace(fileName) == "" {
		// BPS reads the part's Content-Type; the name is only cosmetic.
		fileName = "image.png"
	}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	fields := [][2]string{{"model", model}, {"prompt", prompt},
		{"size", strings.TrimSpace(parsed.Size)}, {"quality", strings.TrimSpace(parsed.Quality)},
		{"background", strings.TrimSpace(parsed.Background)}, {"output_format", strings.TrimSpace(parsed.OutputFormat)}}
	if parsed.N > 1 {
		fields = append(fields, [2]string{"n", strconv.Itoa(parsed.N)})
	}
	for _, field := range fields {
		if field[1] == "" {
			continue
		}
		if err := writer.WriteField(field[0], field[1]); err != nil {
			return nil, "", err
		}
	}
	// CreateFormFile would hardcode application/octet-stream; keep the image MIME.
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="image"; filename="`+excelBPSImagesQuoteEscaper.Replace(fileName)+`"`)
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(data); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

func newExcelBPSImagesRequest(ctx context.Context, upstreamURL, contentType string, body []byte, token, accountID string) (*http.Request, error) {
	req, err := newExcelBPSRequest(ctx, upstreamURL, body, token, accountID)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	for key, value := range map[string]string{
		"X-Openai-Internal-Basispoints-Client-Platform":       "excel",
		"X-Openai-Internal-Basispoints-Client-Editor":         "excel",
		"X-Openai-Internal-Basispoints-Client-Host":           "office",
		"X-Openai-Internal-Basispoints-Client-Runtime":        "desktop",
		"X-Openai-Internal-Basispoints-Client-Platform-Class": "PC",
		"X-Openai-Internal-Basispoints-Office-Host":           "Excel",
		"X-Openai-Internal-Basispoints-Office-Platform":       "PC",
	} {
		req.Header.Set(key, value)
	}
	return req, nil
}

func (s *OpenAIGatewayService) forwardExcelBPSImage(ctx context.Context, c *gin.Context, account *Account,
	parsed *OpenAIImagesRequest, channelMappedModel string,
) (*OpenAIForwardResult, error) {
	started := time.Now()
	model := firstNonEmpty(channelMappedModel, parsed.Model)
	upstreamModel := account.GetMappedModel(model)
	fail := bpsResponseFailureWriter(c, nil)
	if !excelBPSImagesSupportedModel(upstreamModel) || excelBPSImagesUnsupportedReason(parsed) != "" {
		return fail(http.StatusBadRequest, "excel_bps_unsupported_image_request")
	}
	endpoint := basispoints.ImagesGenerationsURL
	mediaType := "application/json"
	var body []byte
	var err error
	if parsed.IsEdits() {
		endpoint = basispoints.ImagesEditsURL
		body, mediaType, err = buildExcelBPSImagesEditsBody(parsed, upstreamModel)
	} else {
		body, err = buildExcelBPSImagesPayload(parsed, upstreamModel)
	}
	if err != nil {
		return fail(http.StatusBadRequest, "excel_bps_invalid_image_request")
	}
	auth, token, proxy, err := s.excelBPSCredential(ctx, account)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "excel_bps_authorization_unavailable")
	}
	upstreamCtx, release := detachUpstreamContext(ctx, s.openAIImageDrainTimeout())
	defer release()
	request, err := newExcelBPSImagesRequest(upstreamCtx, endpoint, mediaType, body, token, account.GetChatGPTAccountID())
	if err != nil {
		return fail(http.StatusBadRequest, "excel_bps_invalid_image_request")
	}
	SetOpsUpstreamModel(c, upstreamModel)
	sent := time.Now()
	response, err := s.sendExcelBPSRequest(request, account, auth, token, proxy)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(sent).Milliseconds())
	if err != nil {
		return fail(http.StatusBadGateway, "excel_bps_image_result_unconfirmed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusTooManyRequests {
		return fail(http.StatusTooManyRequests, "excel_bps_rate_limited")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fail(http.StatusBadGateway, "excel_bps_image_rejected")
	}
	SetActualOpenAIUpstreamEndpoint(c, request.URL.Path)
	usage, count, sizes, err := s.handleCodexDirectImagesNonStreamingResponse(response, c, parsed)
	if err != nil {
		return fail(http.StatusBadGateway, "excel_bps_image_result_unconfirmed")
	}
	return &OpenAIForwardResult{
		RequestID: response.Header.Get("x-request-id"), UpstreamHeaders: response.Header,
		Usage: usage, Model: parsed.Model, UpstreamModel: upstreamModel, UpstreamEndpoint: request.URL.Path,
		Duration: time.Since(started), ImageCount: count, ImageSize: parsed.SizeTier,
		ImageInputSize: parsed.Size, ImageOutputSizes: sizes,
	}, nil
}

func (s *OpenAIGatewayService) uploadExcelBPSAttachment(ctx context.Context, account *Account, auth *OpenAIOAuthService,
	token, proxy string, image basispoints.InlineAttachment,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	reader, contentType, length, err := image.Multipart()
	if err != nil {
		return "", err
	}
	req, err := newExcelBPSRequest(ctx, basispoints.AttachmentsURL, nil, token, account.GetChatGPTAccountID())
	if err != nil {
		return "", err
	}
	req.Body, req.GetBody, req.ContentLength = io.NopCloser(reader), nil, length
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := s.sendExcelBPSRequest(req, account, auth, token, proxy)
	if err != nil {
		return "", fmt.Errorf("Excel BPS attachment result unconfirmed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Excel BPS attachment rejected with HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 || !gjson.ValidBytes(raw) {
		return "", fmt.Errorf("Excel BPS attachment returned an invalid result")
	}
	id := gjson.GetBytes(raw, "openai_file_id").String()
	if !basispoints.ValidAttachmentID(id) {
		return "", fmt.Errorf("Excel BPS attachment file ID is invalid")
	}
	return id, nil
}
