package openaiimages

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
	"github.com/imroc/req/v3"
)

// ProbeRepo 是 AccountProbe 写入 extra 所需的最小仓储接口；
// 与 service.AccountRepository.UpdateExtra 兼容。
type ProbeRepo interface {
	UpdateExtra(ctx context.Context, id int64, updates map[string]any) error
}

// ProbeAccount 是 probe 操作所需的账号最小投影。
type ProbeAccount struct {
	ID          int64
	AccessToken string
	ProxyURL    string // 可选，分组级代理
}

// ProbeResult 描述一次 probe 解析后的关键字段。
type ProbeResult struct {
	Email          string
	AccountPlan    string    // free / plus / pro / team / enterprise / business
	QuotaRemaining int       // image_gen 剩余次数；-1 表示未知
	QuotaTotal     int       // 总次数；0 表示未知（chatgpt 不直接给 total）
	CooldownUntil  time.Time // 限流恢复时间；零值表示无限流
	ProbedAt       time.Time
}

// AccountProbe 调 chatgpt.com 反查 OAuth 账号实时状态。
//
// 协议来源：参照 chatgpt2api `account_service.fetch_remote_info`：
//   - GET  /backend-api/me                    → email + 部分 account_plan
//   - POST /backend-api/conversation/init     → limits_progress（含 image_gen 剩余 / reset_after）
//   - JWT 中 https://api.openai.com/auth.chatgpt_plan_type 优先用作 plan
type AccountProbe struct {
	Client     *req.Client
	HTTPClient *req.Client // 备用，若 Client 为 nil 则用此

	MeURL   string // 默认 https://chatgpt.com/backend-api/me
	InitURL string // 默认 https://chatgpt.com/backend-api/conversation/init

	Repo ProbeRepo // 可选：probe 后自动写库；nil 时仅返回结果
	Now  func() time.Time
}

// NewAccountProbe 创建带默认配置的 probe。
func NewAccountProbe(repo ProbeRepo) *AccountProbe {
	return &AccountProbe{Repo: repo, Now: time.Now}
}

func (p *AccountProbe) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *AccountProbe) httpClient() *req.Client {
	if p.Client != nil {
		return p.Client
	}
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	// 默认 client；调用方实际请求时会基于账号 ID 重建 fingerprint-bound client。
	c, _ := webdriver.NewProbeClient("", webdriver.PickFingerprint(0))
	p.Client = c
	return c
}

func (p *AccountProbe) meURL() string {
	if p.MeURL != "" {
		return p.MeURL
	}
	return "https://chatgpt.com/backend-api/me"
}

func (p *AccountProbe) initURL() string {
	if p.InitURL != "" {
		return p.InitURL
	}
	return "https://chatgpt.com/backend-api/conversation/init"
}

// RefreshAccount 同步发起 me + init 两请求，解析后写入 extra。
//
// 触发场景：
//   - 启动并发 probe（boot-probe）
//   - WebDriver 收到 RateLimitError 后调用以记录 cooldown
//   - ImagePool.SelectAccount 选不到号且 cooldown 已过时
func (p *AccountProbe) RefreshAccount(ctx context.Context, acc ProbeAccount) (*ProbeResult, error) {
	if acc.AccessToken == "" {
		return nil, errors.New("openai-image probe: access token required")
	}

	fp := webdriver.PickFingerprint(acc.ID)
	c, err := webdriver.NewProbeClient(acc.ProxyURL, fp)
	if err != nil {
		return nil, fmt.Errorf("probe client: %w", err)
	}
	// 必须先模拟浏览器导航到 chatgpt.com，让 client 持有 cf_clearance 等 anti-bot cookie；
	// 否则后续 /backend-api/me 会被 CF 直接 403。
	_ = webdriver.PrimeChatGPTSession(ctx, c, fp)
	headers := webdriver.BuildBearerHeadersMap(acc.AccessToken, fp)

	type respBody struct {
		status int
		body   []byte
		err    error
	}
	mePromise := make(chan respBody, 1)
	initPromise := make(chan respBody, 1)

	go func() {
		r := c.R().SetContext(ctx).SetHeaders(headers)
		resp, err := r.Get(p.meURL())
		if err != nil {
			mePromise <- respBody{err: err}
			return
		}
		mePromise <- respBody{status: resp.StatusCode, body: resp.Bytes()}
	}()
	go func() {
		body := map[string]any{
			"gizmo_id":                nil,
			"requested_default_model": nil,
			"conversation_id":         nil,
			"timezone_offset_min":     -480,
		}
		r := c.R().SetContext(ctx).SetHeaders(headers).
			SetBodyJsonMarshal(body)
		resp, err := r.Post(p.initURL())
		if err != nil {
			initPromise <- respBody{err: err}
			return
		}
		initPromise <- respBody{status: resp.StatusCode, body: resp.Bytes()}
	}()

	mr := <-mePromise
	ir := <-initPromise

	if mr.err != nil {
		return nil, fmt.Errorf("probe me: %w", mr.err)
	}
	if mr.status == http.StatusUnauthorized || mr.status == http.StatusForbidden {
		return nil, fmt.Errorf("probe me: auth failed (HTTP %d)", mr.status)
	}
	if mr.status >= 400 {
		return nil, fmt.Errorf("probe me: HTTP %d", mr.status)
	}
	if ir.err != nil {
		return nil, fmt.Errorf("probe init: %w", ir.err)
	}
	if ir.status >= 400 {
		// init 偶尔会因为 ToS 等原因 4xx，但 me 200 时仍可继续；这里降级处理
		ir.body = nil
	}

	res := p.parseProbe(acc.AccessToken, mr.body, ir.body)
	res.ProbedAt = p.now()

	if p.Repo != nil {
		if err := p.write(ctx, acc.ID, res); err != nil {
			return res, fmt.Errorf("probe write extra: %w", err)
		}
	}
	return res, nil
}

func (p *AccountProbe) parseProbe(accessToken string, meBody, initBody []byte) *ProbeResult {
	r := &ProbeResult{QuotaRemaining: -1}

	var me map[string]any
	_ = json.Unmarshal(meBody, &me)
	if v, _ := me["email"].(string); v != "" {
		r.Email = v
	}

	r.AccountPlan = detectAccountPlan(accessToken, me, initBody)

	var init map[string]any
	if len(initBody) > 0 {
		_ = json.Unmarshal(initBody, &init)
	}
	if init != nil {
		if list, ok := init["limits_progress"].([]any); ok {
			extractImageQuota(list, r)
		}
	}
	return r
}

func extractImageQuota(items []any, r *ProbeResult) {
	for _, item := range items {
		obj, _ := item.(map[string]any)
		if obj == nil {
			continue
		}
		if name, _ := obj["feature_name"].(string); name != "image_gen" {
			continue
		}
		if v, ok := obj["remaining"].(float64); ok {
			r.QuotaRemaining = int(v)
		}
		if v, ok := obj["limit"].(float64); ok {
			r.QuotaTotal = int(v)
		}
		// reset_after is the rolling-window reset, not a cooldown.
		// Only treat it as a cooldown when remaining is exhausted.
		if r.QuotaRemaining == 0 {
			if reset, _ := obj["reset_after"].(string); reset != "" {
				if t, err := time.Parse(time.RFC3339, reset); err == nil {
					r.CooldownUntil = t
				}
			}
		}
		return
	}
}

// detectAccountPlan 优先级：JWT > me.account_plan_type > me.account 各种字段 > "free"
func detectAccountPlan(accessToken string, me map[string]any, initBody []byte) string {
	if p := planFromJWT(accessToken); p != "" {
		return p
	}
	if me != nil {
		for _, k := range []string{"account_plan_type", "chatgpt_plan_type", "plan_type"} {
			if v, _ := me[k].(string); v != "" {
				return normalizePlan(v)
			}
		}
		if accObj, ok := me["account"].(map[string]any); ok {
			for _, k := range []string{"plan_type", "subscription_plan", "account_plan"} {
				if v, _ := accObj[k].(string); v != "" {
					return normalizePlan(v)
				}
			}
		}
	}
	return "free"
}

func planFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 标准 base64 兼容
		if pad := len(parts[1]) % 4; pad != 0 {
			parts[1] += strings.Repeat("=", 4-pad)
		}
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	auth, _ := payload["https://api.openai.com/auth"].(map[string]any)
	if auth == nil {
		return ""
	}
	if v, _ := auth["chatgpt_plan_type"].(string); v != "" {
		return normalizePlan(v)
	}
	return ""
}

func normalizePlan(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	switch {
	case strings.Contains(p, "pro"):
		return "pro"
	case strings.Contains(p, "plus"):
		return "plus"
	case strings.Contains(p, "team"):
		return "team"
	case strings.Contains(p, "enterprise"):
		return "enterprise"
	case strings.Contains(p, "business"):
		return "business"
	case p == "":
		return "free"
	default:
		return p
	}
}

func (p *AccountProbe) write(ctx context.Context, id int64, r *ProbeResult) error {
	if r == nil {
		return nil
	}
	updates := map[string]any{
		"image_account_plan":   r.AccountPlan,
		"image_last_probed_at": r.ProbedAt.UTC().Format(time.RFC3339),
	}
	if r.Email != "" {
		updates["account_email"] = r.Email
	}
	if r.QuotaRemaining >= 0 {
		updates["image_quota_remaining"] = r.QuotaRemaining
	}
	if r.QuotaTotal > 0 {
		updates["image_quota_total"] = r.QuotaTotal
	}
	if !r.CooldownUntil.IsZero() {
		updates["image_cooldown_until"] = r.CooldownUntil.UTC().Format(time.RFC3339)
	} else {
		updates["image_cooldown_until"] = ""
	}
	return p.Repo.UpdateExtra(ctx, id, updates)
}

// MarkRateLimited 当 driver 命中 429 时由 pool 调用，仅写 cooldown_until。
func (p *AccountProbe) MarkRateLimited(ctx context.Context, accountID int64, resetAt time.Time) error {
	if p.Repo == nil || resetAt.IsZero() {
		return nil
	}
	return p.Repo.UpdateExtra(ctx, accountID, map[string]any{
		"image_cooldown_until": resetAt.UTC().Format(time.RFC3339),
	})
}

// 进程级最近 probe 时刻表，用于节流：避免短时间内重复打 chatgpt.com。
var (
	probeThrottleMu sync.Mutex
	probeThrottleAt = map[int64]time.Time{}
)

const probeMinInterval = 30 * time.Second

// ShouldProbeNow 节流检查 + 标记。
func ShouldProbeNow(accountID int64, now time.Time) bool {
	probeThrottleMu.Lock()
	defer probeThrottleMu.Unlock()
	last, ok := probeThrottleAt[accountID]
	if ok && now.Sub(last) < probeMinInterval {
		return false
	}
	probeThrottleAt[accountID] = now
	return true
}

// ResetProbeThrottleForTest 测试用。
func ResetProbeThrottleForTest() {
	probeThrottleMu.Lock()
	probeThrottleAt = map[int64]time.Time{}
	probeThrottleMu.Unlock()
}
