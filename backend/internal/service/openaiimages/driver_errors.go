package openaiimages

import (
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// AuthError 表示账号凭证失效（401/403）。dispatch 层应剔除该账号并重试。
type AuthError struct {
	Reason     string
	HTTPStatus int
}

func (e *AuthError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("openai-image auth failed (HTTP %d): %s", e.HTTPStatus, e.Reason)
	}
	return fmt.Sprintf("openai-image auth failed (HTTP %d)", e.HTTPStatus)
}

// RateLimitError 表示账号被限流（429 / chatgpt-cooldown）。
// dispatch 层把 ResetAfter 写入账号 cooldown_until 并切换账号。
type RateLimitError struct {
	ResetAfter time.Duration
	Reason     string
	HTTPStatus int
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("openai-image rate limited (reset_after=%s): %s", e.ResetAfter, e.Reason)
}

// TransportError 表示 5xx / 网络层失败，可重试（dispatch 层切换账号继续）。
type TransportError struct {
	Reason     string
	HTTPStatus int
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("openai-image transport error (HTTP %d): %s", e.HTTPStatus, e.Reason)
}

// UpstreamError 是其他 4xx / 解析失败错误。dispatch 不重试，原样透传给客户端。
type UpstreamError struct {
	HTTPStatus int
	Body       []byte
	Reason     string
}

func (e *UpstreamError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("openai-image upstream error (HTTP %d): %s", e.HTTPStatus, e.Reason)
	}
	return fmt.Sprintf("openai-image upstream error (HTTP %d)", e.HTTPStatus)
}

// IsRetryable 报告 driver 错误是否值得在新账号上重试。
func IsRetryable(err error) bool {
	var rl *RateLimitError
	var tr *TransportError
	var au *AuthError
	switch {
	case errors.As(err, &rl):
		return true
	case errors.As(err, &tr):
		return true
	case errors.As(err, &au):
		return true
	}
	return false
}

// IsAuth 判断是否为账号鉴权失败。
func IsAuth(err error) bool {
	var au *AuthError
	return errors.As(err, &au)
}

// IsRateLimit 判断是否为账号限流。
func IsRateLimit(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl)
}

// encodeBase64 标准 base64 编码（image_url data scheme 用）。
func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// DecodeBase64 标准 base64 解码；自动剥离 "data:...;base64," 前缀。
// handler 在把 ResponseFormat=URL 的图片落入 ImageCache 时复用。
func DecodeBase64(s string) ([]byte, error) {
	if i := indexComma(s); i >= 0 && hasDataPrefix(s) {
		s = s[i+1:]
	}
	return base64.StdEncoding.DecodeString(s)
}

func indexComma(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			return i
		}
	}
	return -1
}

func hasDataPrefix(s string) bool {
	return len(s) >= 5 && (s[:5] == "data:" || s[:5] == "DATA:")
}
