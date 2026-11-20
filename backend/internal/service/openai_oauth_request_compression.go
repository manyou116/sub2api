package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

const (
	openAIOAuthCompressionMaxBytes     = 64 << 20
	openAIOAuthInlineImageMaxBytes     = 8 << 20
	openAIRequestCompressionKey        = "openai_request_compression_enabled"
	openAIRequestCompressionOptionsKey = "openai_request_compression_options"
)

type cachedOpenAIRequestCompression struct {
	enabled bool
	err     error
	until   time.Time
}

type OpenAIRequestCompressionOptions struct {
	ImageGuardEnabled      bool `json:"image_guard_enabled"`
	ImageGuardMaxBytes     int  `json:"image_guard_max_bytes"`
	Bare400FallbackEnabled bool `json:"bare_400_fallback_enabled"`
}

func defaultOpenAIRequestCompressionOptions() OpenAIRequestCompressionOptions {
	return OpenAIRequestCompressionOptions{true, openAIOAuthInlineImageMaxBytes, true}
}

func (o OpenAIRequestCompressionOptions) Validate() error {
	if o.ImageGuardMaxBytes < 1<<20 || o.ImageGuardMaxBytes > openAIOAuthCompressionMaxBytes {
		return errors.New("image protection threshold must be between 1 MiB and 64 MiB")
	}
	return nil
}

type cachedOpenAIRequestCompressionOptions struct {
	options OpenAIRequestCompressionOptions
	err     error
	until   time.Time
}

func (s *SettingService) GetOpenAIRequestCompressionOptions(ctx context.Context) (OpenAIRequestCompressionOptions, error) {
	if s == nil || s.settingRepo == nil {
		return defaultOpenAIRequestCompressionOptions(), nil
	}
	read := func() *cachedOpenAIRequestCompressionOptions {
		v, _ := s.openAIRequestCompressionOptionsCache.Load().(*cachedOpenAIRequestCompressionOptions)
		return v
	}
	if v := read(); v != nil && time.Now().Before(v.until) {
		return v.options, v.err
	}
	s.openAIRequestCompressionOptionsMu.Lock()
	defer s.openAIRequestCompressionOptionsMu.Unlock()
	if v := read(); v != nil && time.Now().Before(v.until) {
		return v.options, v.err
	}
	dbCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	raw, err := s.settingRepo.GetValue(dbCtx, openAIRequestCompressionOptionsKey)
	if errors.Is(err, ErrSettingNotFound) {
		err = nil
	}
	options := defaultOpenAIRequestCompressionOptions()
	if err == nil && raw != "" {
		err = json.Unmarshal([]byte(raw), &options)
	}
	if err == nil {
		err = options.Validate()
	}
	ttl := 30 * time.Second
	if err != nil {
		options, ttl = defaultOpenAIRequestCompressionOptions(), time.Second
		options.Bare400FallbackEnabled = false
	}
	s.openAIRequestCompressionOptionsCache.Store(&cachedOpenAIRequestCompressionOptions{options, err, time.Now().Add(ttl)})
	return options, err
}

func (s *SettingService) SetOpenAIRequestCompressionOptions(ctx context.Context, enabled *bool, options OpenAIRequestCompressionOptions) error {
	if err := options.Validate(); err != nil {
		return err
	}
	s.openAIRequestCompressionMu.Lock()
	defer s.openAIRequestCompressionMu.Unlock()
	s.openAIRequestCompressionOptionsMu.Lock()
	defer s.openAIRequestCompressionOptionsMu.Unlock()
	encoded, err := json.Marshal(options)
	if err != nil {
		return err
	}
	updates := map[string]string{openAIRequestCompressionOptionsKey: string(encoded)}
	if enabled != nil {
		updates[openAIRequestCompressionKey] = strconv.FormatBool(*enabled)
	}
	if err := s.settingRepo.SetMultiple(ctx, updates); err != nil {
		return err
	}
	s.openAIRequestCompressionOptionsCache.Store(&cachedOpenAIRequestCompressionOptions{options: options, until: time.Now().Add(30 * time.Second)})
	if enabled != nil {
		s.openAIRequestCompressionCache.Store(&cachedOpenAIRequestCompression{enabled: *enabled, until: time.Now().Add(30 * time.Second)})
	}
	return nil
}

func (s *SettingService) GetOpenAIRequestCompression(ctx context.Context) (bool, error) {
	if s == nil || s.settingRepo == nil {
		return true, nil
	}
	read := func() *cachedOpenAIRequestCompression {
		v, _ := s.openAIRequestCompressionCache.Load().(*cachedOpenAIRequestCompression)
		return v
	}
	if v := read(); v != nil && time.Now().Before(v.until) {
		return v.enabled, v.err
	}
	s.openAIRequestCompressionMu.Lock()
	defer s.openAIRequestCompressionMu.Unlock()
	if v := read(); v != nil && time.Now().Before(v.until) {
		return v.enabled, v.err
	}
	dbCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	raw, err := s.settingRepo.GetValue(dbCtx, openAIRequestCompressionKey)
	if errors.Is(err, ErrSettingNotFound) {
		err = nil
	}
	enabled := raw == "" || raw == "true"
	if err == nil && raw != "" && raw != "true" && raw != "false" {
		err = errors.New("invalid OpenAI request compression setting")
	}
	ttl := 30 * time.Second
	if err != nil {
		enabled, ttl = false, time.Second
	}
	s.openAIRequestCompressionCache.Store(&cachedOpenAIRequestCompression{enabled, err, time.Now().Add(ttl)})
	return enabled, err
}

func (s *SettingService) SetOpenAIRequestCompression(ctx context.Context, enabled bool) error {
	s.openAIRequestCompressionMu.Lock()
	defer s.openAIRequestCompressionMu.Unlock()
	if err := s.settingRepo.Set(ctx, openAIRequestCompressionKey, strconv.FormatBool(enabled)); err != nil {
		return err
	}
	s.openAIRequestCompressionCache.Store(&cachedOpenAIRequestCompression{enabled: enabled, until: time.Now().Add(30 * time.Second)})
	return nil
}

// doOpenAIOAuthCompressed compresses only the tested official Codex OAuth route.
// Plugin transports own their wire format and are dispatched before this hook.
func (s *OpenAIGatewayService) doOpenAIOAuthCompressed(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	measure := isOpenAIOAuthCompressionRoute(request, account) && request.ContentLength >= 0 && request.Header.Get("Content-Encoding") == ""
	if account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth {
		if enabled, settingErr := s.settingService.GetOpenAIRequestCompression(request.Context()); !enabled {
			if measure {
				reason := "global_disabled"
				if settingErr != nil {
					reason = "settings_unavailable"
				}
				recordUpstreamRequestBody(request.Context(), request.ContentLength, request.ContentLength, "identity", reason, false)
			}
			return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
		}
	}
	options := defaultOpenAIRequestCompressionOptions()
	if measure {
		options, _ = s.settingService.GetOpenAIRequestCompressionOptions(request.Context())
	}
	compressed, reason := prepareOpenAIOAuthCompressedRequestWithImageGuard(request, account, options.ImageGuardEnabled, options.ImageGuardMaxBytes)
	if compressed == nil {
		if measure {
			recordUpstreamRequestBody(request.Context(), request.ContentLength, request.ContentLength, "identity", reason, false)
		}
		return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
	}
	if request.Body != nil {
		_ = request.Body.Close()
	}
	if measure {
		recordUpstreamRequestBody(request.Context(), request.ContentLength, compressed.ContentLength, "zstd", "compressed", false)
	}
	response, err := s.httpUpstream.Do(compressed, proxyURL, account.ID, account.Concurrency)
	// Never replay transport failures or ordinary API errors: the request may
	// already have been accepted. A bounded generic 400 replay is safe only
	// before any downstream stream event has been returned.
	if err != nil || request.Context().Err() != nil {
		return response, err
	}
	rejected, rejectionReason := openAIOAuthEncodingRejected(response, options.Bare400FallbackEnabled)
	if !rejected || request.Context().Err() != nil {
		return response, err
	}
	body, bodyErr := request.GetBody()
	if bodyErr != nil {
		return response, nil
	}
	_ = response.Body.Close()
	plain := request.Clone(request.Context())
	plain.Body = body
	if measure {
		recordUpstreamRequestBody(request.Context(), request.ContentLength, request.ContentLength, "identity", rejectionReason, true)
	}
	return s.httpUpstream.Do(plain, proxyURL, account.ID, account.Concurrency)
}

func isOpenAIOAuthCompressionRoute(request *http.Request, account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth &&
		request != nil && request.URL != nil && request.Method == http.MethodPost &&
		request.URL.Scheme == "https" && request.URL.Host == "chatgpt.com" &&
		(request.Host == "" || request.Host == "chatgpt.com") && request.URL.Path == "/backend-api/codex/responses"
}

func compressOpenAIOAuthRequest(request *http.Request, account *Account) *http.Request {
	compressed, _ := prepareOpenAIOAuthCompressedRequest(request, account)
	return compressed
}

func prepareOpenAIOAuthCompressedRequest(request *http.Request, account *Account) (*http.Request, string) {
	return prepareOpenAIOAuthCompressedRequestWithImageGuard(request, account, true, openAIOAuthInlineImageMaxBytes)
}

func prepareOpenAIOAuthCompressedRequestWithImageGuard(request *http.Request, account *Account, imageGuardEnabled bool, imageMaxBytes int) (*http.Request, string) {
	if !isOpenAIOAuthCompressionRoute(request, account) {
		return nil, "ineligible"
	}
	if value, exists := account.Extra["openai_request_compression"]; exists {
		if enabled, ok := value.(bool); !ok || !enabled {
			return nil, "account_disabled"
		}
	}
	if request.Header.Get("Content-Encoding") != "" || request.GetBody == nil || request.ContentLength < 0 {
		return nil, "ineligible"
	}
	if request.ContentLength > openAIOAuthCompressionMaxBytes {
		return nil, "size_limit"
	}
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && !strings.HasSuffix(contentType, "+json")) {
		return nil, "non_json"
	}
	replay, err := request.GetBody()
	if err != nil {
		return nil, "read_error"
	}
	plain, err := io.ReadAll(io.LimitReader(replay, openAIOAuthCompressionMaxBytes+1))
	_ = replay.Close()
	if err != nil {
		return nil, "read_error"
	}
	if len(plain) > openAIOAuthCompressionMaxBytes {
		return nil, "size_limit"
	}
	if imageGuardEnabled && len(plain) > imageMaxBytes && containsOpenAIInlineImage(plain) {
		return nil, "inline_image_size_limit"
	}
	if gjson.GetBytes(plain, "stream").Type != gjson.True {
		return nil, "non_stream"
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, "compression_error"
	}
	encoded := encoder.EncodeAll(plain, nil)
	_ = encoder.Close()
	if len(encoded) >= len(plain) {
		return nil, "not_smaller"
	}
	compressed := request.Clone(request.Context())
	compressed.Body = io.NopCloser(bytes.NewReader(encoded))
	compressed.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }
	compressed.ContentLength = int64(len(encoded))
	compressed.TransferEncoding = nil
	compressed.Header.Del("Content-Length")
	compressed.Header.Set("Content-Encoding", "zstd")
	return compressed, "compressed"
}

type openAIOAuthReplayResponseBody struct {
	io.Reader
	io.Closer
}

func containsOpenAIInlineImage(body []byte) bool {
	return bytes.Contains(body, []byte(`"input_image"`)) && bytes.Contains(body, []byte(`data:image/`))
}

func openAIOAuthEncodingRejected(response *http.Response, allowBare400 bool) (bool, string) {
	if response == nil || response.Body == nil {
		return false, ""
	}
	if response.StatusCode == http.StatusUnsupportedMediaType {
		return true, "encoding_rejected"
	}
	if response.StatusCode != http.StatusBadRequest {
		return false, ""
	}
	if strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return false, ""
	}
	// Preserve the complete error for existing handlers, even if it exceeds
	// this bounded inspection window or reading it fails partway through.
	prefix, err := io.ReadAll(io.LimitReader(response.Body, (8<<10)+1))
	response.Body = &openAIOAuthReplayResponseBody{Reader: io.MultiReader(bytes.NewReader(prefix), response.Body), Closer: response.Body}
	message := strings.ToLower(string(prefix))
	if err == nil && strings.Contains(message, "zstd") &&
		strings.Contains(message, "content-encoding") &&
		(strings.Contains(message, "unsupported") || strings.Contains(message, "decompress")) {
		return true, "encoding_rejected"
	}
	var compact bytes.Buffer
	if allowBare400 && err == nil && len(prefix) <= 8<<10 && json.Compact(&compact, prefix) == nil && bytes.Equal(compact.Bytes(), []byte(`{"detail":"Bad Request"}`)) {
		return true, "bare_400"
	}
	return false, ""
}
