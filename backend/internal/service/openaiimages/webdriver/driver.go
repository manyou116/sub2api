package webdriver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/imroc/req/v3"
	"go.uber.org/zap"

	pkglogger "github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// Driver 是 web 反代图片网关的对外入口。
//
// 本身无状态：每次 Forward 创建一个新的 imroc/req client（带账号代理）+ 对应账号 headers。
// sentinel SDK 资源使用进程级 5min 缓存以避免每次都 GET chatgpt.com 首页。
type Driver struct {
	endpoints Endpoints // 测试可注入；零值使用真实 chatgpt.com 端点
}

// Endpoints 把所有 chatgpt.com URL 集中为可注入字段，方便 httptest 替换。
type Endpoints struct {
	Start              string
	ConversationInit   string
	Conversation       string
	ConversationPrep   string
	ChatRequirements   string
	Files              string
	BaseConversation   string // /backend-api/conversation （poll/attachment 拼接用）
}

func (e Endpoints) start() string              { return coalesce(e.Start, startURL) }
func (e Endpoints) convInit() string           { return coalesce(e.ConversationInit, conversationInitURL) }
func (e Endpoints) conv() string               { return coalesce(e.Conversation, conversationURL) }
func (e Endpoints) prep() string               { return coalesce(e.ConversationPrep, conversationPrepareURL) }
func (e Endpoints) reqs() string               { return coalesce(e.ChatRequirements, chatRequirementsURL) }
func (e Endpoints) files() string              { return coalesce(e.Files, filesURL) }
func (e Endpoints) baseConv() string {
	return coalesce(e.BaseConversation, "https://chatgpt.com/backend-api/conversation")
}

// New 创建一个 Driver。endpoints 为零值时使用真实 chatgpt.com 端点。
func New(endpoints Endpoints) *Driver {
	return &Driver{endpoints: endpoints}
}

// Forward 执行完整的生图 / 改图流程。
//
// 失败语义：所有上游异常均落到 typed error（RateLimitError / AuthError /
// ProtocolError / TransportError），上层根据 error 类型做换号 / 限流回写。
func (d *Driver) Forward(ctx context.Context, in *Request) (*Result, error) {
	if in == nil || in.Account.AccessToken == "" {
		return nil, errors.New("webdriver: missing access token")
	}
	startTime := time.Now()

	fp := PickFingerprint(in.Account.AccountID)
	client, err := newHTTPClient(in.Account.ProxyURL, fp)
	if err != nil {
		return nil, err
	}
	headers := buildHeaders(in.Account, fp)
	bootstrapHdrs := buildBootstrapHeaders(in.Account, fp)

	scriptSources, dataBuild := bootstrap(ctx, client, bootstrapHdrs, d.endpoints.start())

	reqs, err := fetchChatRequirements(ctx, client, headers, d.endpoints.reqs(), scriptSources, dataBuild)
	if err != nil {
		return nil, err
	}
	if reqs.Arkose.Required {
		return nil, &ProtocolError{Reason: "arkose challenge required (account flagged)"}
	}

	ua := headers.Get("User-Agent")
	proofToken, err := buildProofToken(reqs.ProofOfWork.Required, reqs.ProofOfWork.Seed, reqs.ProofOfWork.Difficulty, ua, scriptSources, dataBuild)
	if err != nil {
		return nil, &ProtocolError{Reason: err.Error()}
	}

	parentMessageID := uuid.NewString()
	_ = initConversation(ctx, client, headers, d.endpoints.convInit())

	uploads, err := uploadFiles(ctx, client, headers, d.endpoints.files(), in.Uploads)
	if err != nil {
		return nil, err
	}
	excludedPointers := buildUploadPointerSet(uploads)
	// pointer-level 去重已通过 buildUploadPointerSet（file-service:// + sediment://）保证，
	// 不再做 sha256 内容去重（chatgpt2api 也未做，避免误杀视觉相近的合法 edit 结果）。

	prompt := buildPrompt(in.Prompt, len(uploads) > 0)
	conduitToken, err := prepareConversation(ctx, client, headers, d.endpoints.prep(), prompt, parentMessageID, reqs.Token, proofToken, in.Model)
	if err != nil {
		return nil, err
	}

	convPayload := buildConversationPayload(in.Model, prompt, parentMessageID, uploads)
	convHeaders := cloneHTTPHeader(headers)
	convHeaders.Set("Accept", "text/event-stream")
	convHeaders.Set("Content-Type", "application/json")
	convHeaders.Set("openai-sentinel-chat-requirements-token", reqs.Token)
	if proofToken != "" {
		convHeaders.Set("openai-sentinel-proof-token", proofToken)
	}
	if conduitToken != "" {
		convHeaders.Set("X-Conduit-Token", conduitToken)
	}
	convHeaders.Set("X-Oai-Turn-Trace-Id", uuid.NewString())

	expectedN := in.N
	if expectedN < 1 {
		expectedN = 1
	}
	resp, err := client.R().
		SetContext(ctx).
		DisableAutoReadResponse().
		SetHeaders(headerToMap(convHeaders)).
		SetBodyJsonMarshal(convPayload).
		Post(d.endpoints.conv())
	if err != nil {
		return nil, &TransportError{Wrapped: fmt.Errorf("conversation: %w", err)}
	}
	streamHandedOff := false
	defer func() {
		if !streamHandedOff && resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.StatusCode >= 400 {
		return nil, classifyHTTPError(resp, "conversation request failed")
	}

	allowEarlyExit := in.AllowEarlyExit
	conversationID, ptrs, firstTokenMs, earlyExit, sseErr := readSSE(resp, startTime, expectedN, excludedPointers, allowEarlyExit)
	if sseErr != nil {
		return nil, sseErr
	}
	if earlyExit {
		streamHandedOff = true
	}

	// 兜底轮询：仅当 SSE 没拿到任何可下载 pointer 时触发。
	// （edits 同样适用：source pointers 已通过 excludedPointers 排除，
	// SSE 中拿到的非 source pointer 即为模型生成结果，无需再 poll。）
	if conversationID != "" && countDownloadablePointers(ptrs) == 0 {
		pollCtx, cancel := detachContext(ctx, lifecycleTimeout)
		polled, perr := pollConversation(pollCtx, client, headers, d.endpoints.baseConv(), conversationID, excludedPointers, len(in.Uploads) == 0)
		cancel()
		if perr != nil {
			return nil, perr
		}
		ptrs = mergePointers(ptrs, polled)
	}
	ptrs = preferFileService(ptrs)
	if len(ptrs) == 0 {
		return nil, &ProtocolError{Reason: "no downloadable image pointers", ConversationID: conversationID}
	}

	// 下载预算：每个 pointer 独立 45s 上限，整体最多 max(60s, 45s*N)。
	// 之前所有 pointer 共享 30s 容易在第一个 pointer 慢速时把后续全部饿死，
	// 加上下载阶段已加 retry，整体放宽以提升成功率。
	perPointerBudget := 45 * time.Second
	overall := perPointerBudget * time.Duration(len(ptrs))
	if overall < 60*time.Second {
		overall = 60 * time.Second
	}
	dlCtx, dlCancel := detachContext(ctx, overall)
	images, err := d.downloadAll(dlCtx, client, headers, conversationID, ptrs, perPointerBudget)
	dlCancel()
	if err != nil {
		return nil, err
	}
	if len(images) == 0 {
		return nil, &ProtocolError{Reason: fmt.Sprintf("downloads failed for %d pointer(s)", len(ptrs)), ConversationID: conversationID}
	}

	return &Result{
		ConversationID: conversationID,
		Images:         images,
		FirstTokenMs:   firstTokenMs,
		Duration:       time.Since(startTime),
		RequestID:      resp.Header.Get("x-request-id"),
	}, nil
}

func (d *Driver) downloadAll(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	conversationID string,
	pointers []pointerInfo,
	perPointerBudget time.Duration,
) ([]Image, error) {
	out := make([]Image, 0, len(pointers))
	for i, p := range pointers {
		pCtx, pCancel := context.WithTimeout(ctx, perPointerBudget)
		downloadURL, err := fetchDownloadURL(pCtx, client, headers, d.endpoints.files(), d.endpoints.baseConv(), conversationID, p.Pointer)
		if err != nil {
			pCancel()
			pkglogger.L().Warn("openaiimages.download_url_failed",
				zap.String("conversation_id", conversationID),
				zap.Int("pointer_index", i),
				zap.String("pointer", truncate(p.Pointer, 120)),
				zap.String("error", err.Error()),
			)
			continue
		}
		data, ct, err := downloadBytes(pCtx, client, headers, downloadURL)
		pCancel()
		if err != nil {
			pkglogger.L().Warn("openaiimages.download_bytes_failed",
				zap.String("conversation_id", conversationID),
				zap.Int("pointer_index", i),
				zap.String("pointer", truncate(p.Pointer, 120)),
				zap.String("download_url", truncate(downloadURL, 200)),
				zap.String("error", err.Error()),
			)
			continue
		}
		out = append(out, Image{Bytes: data, ContentType: ct, Pointer: p.Pointer})
	}
	return out, nil
}

// detachContext 在外部 ctx 已结束时仍允许后续清理工作完成。
func detachContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		return context.WithTimeout(context.Background(), timeout)
	}
	return context.WithTimeout(context.Background(), timeout)
}

func mergePointers(a, b []pointerInfo) []pointerInfo {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]pointerInfo, 0, len(a)+len(b))
	for _, x := range a {
		if _, ok := seen[x.Pointer]; ok {
			continue
		}
		seen[x.Pointer] = struct{}{}
		out = append(out, x)
	}
	for _, x := range b {
		if _, ok := seen[x.Pointer]; ok {
			continue
		}
		seen[x.Pointer] = struct{}{}
		out = append(out, x)
	}
	return out
}

// preferFileService 优先保留 file-service:// pointer（直接下载更可靠）。
// 若同时存在两类，sediment:// 全部丢弃。
func preferFileService(items []pointerInfo) []pointerInfo {
	hasFS := false
	for _, it := range items {
		if len(it.Pointer) >= len("file-service://") && it.Pointer[:len("file-service://")] == "file-service://" {
			hasFS = true
			break
		}
	}
	if !hasFS {
		return items
	}
	out := items[:0]
	for _, it := range items {
		if len(it.Pointer) >= len("file-service://") && it.Pointer[:len("file-service://")] == "file-service://" {
			out = append(out, it)
		}
	}
	return out
}
