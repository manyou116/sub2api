package webdriver

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/imroc/req/v3"
	"github.com/tidwall/gjson"
)

// 四类 typed error，给上层 pool / dispatch 做精确错误分支。

// RateLimitError 表示账号被上游限流。ResetAfter 来自上游 reset_after 字段（秒）。
type RateLimitError struct {
	StatusCode  int
	Message     string
	ResetAfter  time.Duration
	RawBody     string
}

func (e *RateLimitError) Error() string {
	if e.ResetAfter > 0 {
		return fmt.Sprintf("openai web rate limit (status=%d, reset_after=%s): %s", e.StatusCode, e.ResetAfter, e.Message)
	}
	return fmt.Sprintf("openai web rate limit (status=%d): %s", e.StatusCode, e.Message)
}

// ProtocolError 表示上游协议偏离预期（拿到文本响应、无法生成图片等），
// 通常意味着该账号能力下线或风控生效，应该换号。
type ProtocolError struct {
	Reason         string
	ConversationID string
}

func (e *ProtocolError) Error() string {
	if e.ConversationID != "" {
		return fmt.Sprintf("openai web protocol error: %s (conversation=%s)", e.Reason, e.ConversationID)
	}
	return "openai web protocol error: " + e.Reason
}

// AuthError 表示账号 access token 失效（401/403）。上层应触发刷新 token 或换号。
type AuthError struct {
	StatusCode int
	Message    string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("openai web auth error (status=%d): %s", e.StatusCode, e.Message)
}

// TransportError 表示底层网络/超时等可重试错误。
type TransportError struct{ Wrapped error }

func (e *TransportError) Error() string { return "openai web transport error: " + e.Wrapped.Error() }
func (e *TransportError) Unwrap() error { return e.Wrapped }

// classifyHTTPError 把 req.Response.IsErrorState 的响应分类为合适的 typed error。
// fallback 给出错误信息默认值。
func classifyHTTPError(resp *req.Response, fallback string) error {
	if resp == nil {
		return errors.New(fallback)
	}
	body, _ := resp.ToBytes()
	bodyStr := string(body)
	msg := extractErrorMessage(body, fallback)
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		lower := strings.ToLower(bodyStr)
		// Cloudflare 挑战 / 边缘屏蔽返回 HTML（典型特征：<html / cloudflare / cf-ray header）。
		// 这类 403 不是账号鉴权失败，而是网络层屏蔽，应作为可重试 TransportError 处理，
		// 否则会把所有正常账号一个个写 1h auth-cooldown 全部废掉。
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		isHTML := strings.Contains(ct, "text/html") || strings.HasPrefix(strings.TrimSpace(lower), "<html") || strings.HasPrefix(strings.TrimSpace(lower), "<!doctype")
		isCF := resp.Header.Get("CF-Ray") != "" || strings.Contains(strings.ToLower(resp.Header.Get("Server")), "cloudflare")
		if isHTML && isCF {
			return &TransportError{Wrapped: fmt.Errorf("cloudflare challenge (status=%d, cf-ray=%s)", resp.StatusCode, resp.Header.Get("CF-Ray"))}
		}
		// 进一步识别 web 限流提示（forbidden + cooldown 文案）
		if strings.Contains(lower, "rate limit") || strings.Contains(lower, "cooldown") {
			return &RateLimitError{StatusCode: resp.StatusCode, Message: msg, RawBody: bodyStr, ResetAfter: extractResetAfter(body)}
		}
		return &AuthError{StatusCode: resp.StatusCode, Message: msg}
	case http.StatusTooManyRequests:
		return &RateLimitError{
			StatusCode: resp.StatusCode,
			Message:    msg,
			RawBody:    bodyStr,
			ResetAfter: extractResetAfter(body),
		}
	}
	if resp.StatusCode >= 500 {
		return &TransportError{Wrapped: fmt.Errorf("upstream %d: %s", resp.StatusCode, msg)}
	}
	return fmt.Errorf("openai web %d: %s", resp.StatusCode, msg)
}

func extractErrorMessage(body []byte, fallback string) string {
	if len(body) == 0 {
		return fallback
	}
	for _, path := range []string{"detail.message", "detail", "error.message", "error", "message"} {
		if v := gjson.GetBytes(body, path).String(); v != "" {
			return v
		}
	}
	return fallback
}

func extractResetAfter(body []byte) time.Duration {
	if len(body) == 0 {
		return 0
	}
	for _, path := range []string{"detail.reset_after", "reset_after", "detail.clears_in", "clears_in"} {
		if v := gjson.GetBytes(body, path).Float(); v > 0 {
			return time.Duration(v) * time.Second
		}
	}
	return 0
}

// IsRetryable 上层重试调度时使用：transport / 5xx / rate-limit 都允许换号重试。
func IsRetryable(err error) bool {
	var (
		rateErr *RateLimitError
		trErr   *TransportError
	)
	return errors.As(err, &rateErr) || errors.As(err, &trErr)
}

// IsAuth 区分账号 token 失效。
func IsAuth(err error) bool {
	var authErr *AuthError
	return errors.As(err, &authErr)
}
