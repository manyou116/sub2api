package service

import (
	"bytes"
	"context"
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
	openAIOAuthCompressionMinBytes = 64 << 10
	openAIOAuthCompressionMaxBytes = 64 << 20
	openAIRequestCompressionKey    = "openai_request_compression_enabled"
)

type cachedOpenAIRequestCompression struct {
	enabled bool
	err     error
	until   time.Time
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
	if account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth {
		if enabled, _ := s.settingService.GetOpenAIRequestCompression(request.Context()); !enabled {
			return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
		}
	}
	compressed := compressOpenAIOAuthRequest(request, account)
	if compressed == nil {
		return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
	}
	if request.Body != nil {
		_ = request.Body.Close()
	}
	response, err := s.httpUpstream.Do(compressed, proxyURL, account.ID, account.Concurrency)
	// Never replay transport failures or ordinary API errors: the request may
	// already have been accepted. Only explicit encoding rejection is safe.
	if err != nil || !openAIOAuthEncodingRejected(response) || request.Context().Err() != nil {
		return response, err
	}
	body, bodyErr := request.GetBody()
	if bodyErr != nil {
		return response, nil
	}
	_ = response.Body.Close()
	plain := request.Clone(request.Context())
	plain.Body = body
	return s.httpUpstream.Do(plain, proxyURL, account.ID, account.Concurrency)
}

func compressOpenAIOAuthRequest(request *http.Request, account *Account) *http.Request {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return nil
	}
	if value, exists := account.Extra["openai_request_compression"]; exists {
		if enabled, ok := value.(bool); !ok || !enabled {
			return nil
		}
	}
	if request == nil || request.URL == nil || request.Method != http.MethodPost ||
		request.URL.Scheme != "https" || request.URL.Host != "chatgpt.com" ||
		(request.Host != "" && request.Host != "chatgpt.com") ||
		request.URL.Path != "/backend-api/codex/responses" ||
		request.Header.Get("Content-Encoding") != "" || request.GetBody == nil ||
		request.ContentLength < openAIOAuthCompressionMinBytes || request.ContentLength > openAIOAuthCompressionMaxBytes {
		return nil
	}
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && !strings.HasSuffix(contentType, "+json")) {
		return nil
	}
	replay, err := request.GetBody()
	if err != nil {
		return nil
	}
	plain, err := io.ReadAll(io.LimitReader(replay, openAIOAuthCompressionMaxBytes+1))
	_ = replay.Close()
	if err != nil || len(plain) < openAIOAuthCompressionMinBytes || len(plain) > openAIOAuthCompressionMaxBytes ||
		gjson.GetBytes(plain, "stream").Type != gjson.True {
		return nil
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil
	}
	encoded := encoder.EncodeAll(plain, nil)
	_ = encoder.Close()
	if len(encoded) >= len(plain) {
		return nil
	}
	compressed := request.Clone(request.Context())
	compressed.Body = io.NopCloser(bytes.NewReader(encoded))
	compressed.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }
	compressed.ContentLength = int64(len(encoded))
	compressed.TransferEncoding = nil
	compressed.Header.Del("Content-Length")
	compressed.Header.Set("Content-Encoding", "zstd")
	return compressed
}

type openAIOAuthReplayResponseBody struct {
	io.Reader
	io.Closer
}

func openAIOAuthEncodingRejected(response *http.Response) bool {
	if response == nil || response.Body == nil {
		return false
	}
	if response.StatusCode == http.StatusUnsupportedMediaType {
		return true
	}
	if response.StatusCode != http.StatusBadRequest {
		return false
	}
	// Preserve the complete error for existing handlers, even if it exceeds
	// this bounded inspection window or reading it fails partway through.
	prefix, err := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	response.Body = &openAIOAuthReplayResponseBody{Reader: io.MultiReader(bytes.NewReader(prefix), response.Body), Closer: response.Body}
	message := strings.ToLower(string(prefix))
	return err == nil && strings.Contains(message, "zstd") &&
		strings.Contains(message, "content-encoding") &&
		(strings.Contains(message, "unsupported") || strings.Contains(message, "decompress"))
}
