// Kiro 上游错误分类。
//
// 来源（权威，2026-05 交叉验证）：
//   - aws/aws-toolkit-vscode user-service-2.json（AWS CodeWhisperer 官方 Smithy schema）
//   - mikeyobrien/pi-provider-kiro src/retry.ts
//   - kkddytd/claude-api internal/amazonq/client.go
//
// 上游真实错误结构（AWS CodeWhisperer / Kiro 共用同一后端 API）：
//
//	HTTP 429 ThrottlingException
//	  reason: DAILY_REQUEST_COUNT | MONTHLY_REQUEST_COUNT | INSUFFICIENT_MODEL_CAPACITY
//
//	HTTP 402 ServiceQuotaExceededException
//	  reason: CONVERSATION_LIMIT_EXCEEDED | MONTHLY_REQUEST_COUNT | OVERAGE_REQUEST_LIMIT_EXCEEDED
//
//	HTTP 403 AccessDeniedException
//	  reason: TEMPORARILY_SUSPENDED | UNAUTHORIZED_* | FEATURE_NOT_SUPPORTED
//
//	HTTP 400 ValidationException
//	  reason: CONTENT_LENGTH_EXCEEDS_THRESHOLD | INVALID_MODEL_ID | INVALID_CONVERSATION_ID
//
// JSON shape: {"__type":"...Exception","message":"...","reason":"..."}
// 部分场景 __type 不带（如 EventStream 中的错误帧），需要 string-contains 兜底。
package service

import (
	"encoding/json"
	"net/http"
	"strings"
)

// KiroErrorClass 上游错误归属维度，决定隔离策略与是否切号。
type KiroErrorClass int

const (
	// KiroErrUnknown 未识别（保守按 Transient 处理）
	KiroErrUnknown KiroErrorClass = iota

	// KiroErrModelCapacity 模型容量不足（INSUFFICIENT_MODEL_CAPACITY）
	// → (account,model) 维度短退避；账号其他模型不受影响；会切号尝试其他账号
	//   （同 (account,model) 短时 flood 则透传 429 不再切号）
	KiroErrModelCapacity

	// KiroErrAccountQuotaDaily 账号日配额耗尽（DAILY_REQUEST_COUNT）
	// → 账号至次日 00:00 UTC；切其他账号
	KiroErrAccountQuotaDaily

	// KiroErrAccountQuotaMonthly 账号月配额耗尽（MONTHLY_REQUEST_COUNT / OVERAGE_REQUEST_LIMIT_EXCEEDED）
	// → 账号至下月 1 日（in-memory）；切其他账号
	KiroErrAccountQuotaMonthly

	// KiroErrConversationTooLong 单会话上下文超限（CONVERSATION_LIMIT_EXCEEDED）
	// → 不隔离；原样回客户端（提示用户开新会话）
	KiroErrConversationTooLong

	// KiroErrAccountSuspended 账号被临时封禁（TEMPORARILY_SUSPENDED）
	// → 账号级 cooldown；切其他账号
	KiroErrAccountSuspended

	// KiroErrAccessDenied 其他权限拒绝（UNAUTHORIZED_*, FEATURE_NOT_SUPPORTED）
	// → 账号 30min cooldown（多半 Pro/SKU 配错，不会很快好）；切其他账号
	KiroErrAccessDenied

	// KiroErrInvalidRequest 客户端请求本身有问题（INVALID_MODEL_ID, CONTENT_LENGTH_EXCEEDS_THRESHOLD, Improperly formed）
	// → 不隔离；原样回客户端
	KiroErrInvalidRequest

	// KiroErrAuth Token 失效（401/403 + invalid bearer token / ExpiredToken）
	// → ForceRefresh 兜底；失败再 5min cooldown 切号
	KiroErrAuth

	// KiroErrTransient 5xx / 网络抖动 / 未识别错误
	// → 不隔离；切其他账号本次重试
	KiroErrTransient
)

// String 返回分类名（用于日志与 metrics）
func (c KiroErrorClass) String() string {
	switch c {
	case KiroErrModelCapacity:
		return "model_capacity"
	case KiroErrAccountQuotaDaily:
		return "account_quota_daily"
	case KiroErrAccountQuotaMonthly:
		return "account_quota_monthly"
	case KiroErrConversationTooLong:
		return "conversation_too_long"
	case KiroErrAccountSuspended:
		return "account_suspended"
	case KiroErrAccessDenied:
		return "access_denied"
	case KiroErrInvalidRequest:
		return "invalid_request"
	case KiroErrAuth:
		return "auth"
	case KiroErrTransient:
		return "transient"
	default:
		return "unknown"
	}
}

// kiroErrorPayload Kiro/AWS 标准错误 JSON。
type kiroErrorPayload struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

// extractTypeAndReason 从 body 提取 (exceptionType, reason)，全部转 lowercase。
// __type 形如 "com.amazon.coral.service#ThrottlingException"，提取 # 后面部分。
func extractTypeAndReason(body []byte) (string, string) {
	var p kiroErrorPayload
	if err := json.Unmarshal(body, &p); err == nil {
		t := p.Type
		if idx := strings.LastIndex(t, "#"); idx >= 0 {
			t = t[idx+1:]
		}
		return strings.ToLower(t), strings.ToLower(p.Reason)
	}
	return "", ""
}

// ClassifyKiroError 把上游 (status, body) 归一到 KiroErrorClass。
//
// 判别顺序：
//  1. JSON __type + reason（最可靠，AWS schema 强制字段）
//  2. body 关键字 fallback（应对 EventStream 错误帧 / 未结构化响应）
//  3. HTTP status 兜底
func ClassifyKiroError(status int, body []byte) KiroErrorClass {
	excType, reason := extractTypeAndReason(body)
	low := strings.ToLower(string(body))

	// === Phase 1: 结构化 reason 优先（最高可信度）===
	switch reason {
	case "insufficient_model_capacity":
		return KiroErrModelCapacity
	case "daily_request_count":
		return KiroErrAccountQuotaDaily
	case "monthly_request_count", "overage_request_limit_exceeded":
		return KiroErrAccountQuotaMonthly
	case "conversation_limit_exceeded":
		return KiroErrConversationTooLong
	case "temporarily_suspended":
		return KiroErrAccountSuspended
	case "unauthorized_customization_resource_access",
		"unauthorized_workspace_context_feature_access",
		"feature_not_supported":
		return KiroErrAccessDenied
	case "content_length_exceeds_threshold",
		"invalid_model_id",
		"invalid_conversation_id":
		return KiroErrInvalidRequest
	}

	// === Phase 2: 仅看 __type（reason 缺失场景）===
	switch excType {
	case "throttlingexception":
		// 没 reason 的 throttling 通常是 capacity；但保守按 capacity（不污染账号）
		return KiroErrModelCapacity
	case "servicequotaexceededexception":
		return KiroErrAccountQuotaMonthly
	case "accessdeniedexception":
		return KiroErrAccessDenied
	case "validationexception":
		return KiroErrInvalidRequest
	case "expiredtokenexception", "unauthorizedexception", "invalidsignatureexception":
		return KiroErrAuth
	}

	// === Phase 3: body 关键字（EventStream / 非标准响应兜底）===
	switch {
	case strings.Contains(low, "insufficient_model_capacity"),
		strings.Contains(low, "high traffic"):
		return KiroErrModelCapacity
	case strings.Contains(low, "monthly_request_count"),
		strings.Contains(low, "overage_request_limit"):
		return KiroErrAccountQuotaMonthly
	case strings.Contains(low, "daily_request_count"):
		return KiroErrAccountQuotaDaily
	case strings.Contains(low, "conversation_limit_exceeded"):
		return KiroErrConversationTooLong
	case strings.Contains(low, "temporarily_suspended"),
		strings.Contains(low, "temporarily is suspended"):
		return KiroErrAccountSuspended
	case strings.Contains(low, "content_length_exceeds_threshold"),
		strings.Contains(low, "improperly formed"),
		strings.Contains(low, "invalid_model_id"),
		strings.Contains(low, "invalid model"):
		return KiroErrInvalidRequest
	case strings.Contains(low, "expiredtoken"),
		strings.Contains(low, "expired token"),
		strings.Contains(low, "invalid bearer token"),
		strings.Contains(low, "bearer token") && strings.Contains(low, "invalid"),
		strings.Contains(low, "invalidsignature"),
		strings.Contains(low, "token expired"):
		return KiroErrAuth
	}

	// === Phase 4: HTTP status 兜底 ===
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// 走到这一定不是 invalid_token 类（前面已被 Phase 3 拦了），
		// 视为权限问题
		return KiroErrAccessDenied
	case status == http.StatusTooManyRequests:
		// 没结构化信息的 429，保守按 capacity（更轻的处理）
		return KiroErrModelCapacity
	case status == http.StatusPaymentRequired: // 402
		return KiroErrAccountQuotaMonthly
	case status == 413 || status == http.StatusBadRequest:
		return KiroErrInvalidRequest
	case status >= 500 && status < 600:
		return KiroErrTransient
	case status == 0 || status >= 400:
		return KiroErrTransient
	}
	return KiroErrUnknown
}
