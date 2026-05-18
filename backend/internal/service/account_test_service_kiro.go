package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// testKiroAccountConnection tests a Kiro account via:
//  1. token refresh (writes back rotated tokens)
//  2. usage probe (writes back usageData)
//  3. (optional) one real chat round-trip to verify model availability
//
// 输出与其他平台一致的 SSE 事件流（test_start / token_refreshed / usage / model_response / test_complete）。
func (s *AccountTestService) testKiroAccountConnection(c *gin.Context, account *Account, modelID, prompt string) error {
	ctx := c.Request.Context()

	// SSE headers
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	startModel := modelID
	if startModel == "" {
		startModel = "kiro"
	}
	s.sendEvent(c, TestEvent{Type: "test_start", Model: startModel})

	tokenSvc := NewKiroTokenService()
	usageSvc := NewKiroUsageService()

	tokenInfo, err := tokenSvc.RefreshAccountToken(ctx, account, "")
	if err != nil {
		return s.sendErrorAndEnd(c, "kiro refresh failed: "+err.Error())
	}
	creds := ApplyKiroTokenInfo(account, tokenInfo)
	account.Credentials = creds
	if persistErr := s.persistKiroCredentials(ctx, account.ID, creds); persistErr != nil {
		return s.sendErrorAndEnd(c, "kiro persist token failed: "+persistErr.Error())
	}
	s.sendEvent(c, TestEvent{Type: "token_refreshed", Status: "ok"})

	usage, err := usageSvc.ProbeAccountUsage(ctx, account, "")
	if err != nil {
		// 401 重试一次
		if errors.Is(err, ErrKiroAuthFailed) {
			tokenInfo2, retryErr := tokenSvc.RefreshAccountToken(ctx, account, "")
			if retryErr != nil {
				return s.sendErrorAndEnd(c, "kiro retry refresh failed: "+retryErr.Error())
			}
			creds = ApplyKiroTokenInfo(account, tokenInfo2)
			account.Credentials = creds
			_ = s.persistKiroCredentials(ctx, account.ID, creds)
			usage, err = usageSvc.ProbeAccountUsage(ctx, account, "")
		}
	}
	if err != nil {
		return s.sendErrorAndEnd(c, "kiro probe usage failed: "+err.Error())
	}

	newExtra := ApplyKiroUsageData(account, usage)
	account.Extra = newExtra
	if persistErr := s.persistKiroExtra(ctx, account.ID, newExtra); persistErr != nil {
		// 仅记录到事件，不阻断
		s.sendEvent(c, TestEvent{Type: "warning", Text: "persist usage failed: " + persistErr.Error()})
	}

	s.sendEvent(c, TestEvent{
		Type:    "usage",
		Data:    usage,
		Status:  kiroCappedLabel(account),
		Success: true,
	})

	// 如果指定了模型，跑一次真实 chat 验证模型可用性
	if modelID != "" {
		testPrompt := strings.TrimSpace(prompt)
		if testPrompt == "" {
			testPrompt = "hi"
		}
		text, chatErr := s.kiroChatProbeStream(ctx, c, account, modelID, testPrompt)
		if chatErr != nil {
			s.sendEvent(c, TestEvent{
				Type:    "content",
				Text:    "[error] " + chatErr.Error(),
				Status:  "error",
				Success: false,
			})
			s.sendEvent(c, TestEvent{Type: "test_complete", Status: "error", Success: false, Error: chatErr.Error()})
			return nil
		}
		_ = text
	}

	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true, Status: "ok"})
	return nil
}

// kiroChatProbeStream 发起一次真实 Kiro chat 调用，每收到一段 delta 就立刻向
// gin.Context 的 SSE 流写一个 content 事件，给 web UI 展现真实的流式输出。
func (s *AccountTestService) kiroChatProbeStream(ctx context.Context, c *gin.Context, account *Account, modelID, prompt string) (string, error) {
	return s.kiroChatProbeCore(ctx, c, account, modelID, prompt)
}

func (s *AccountTestService) kiroChatProbeCore(ctx context.Context, c *gin.Context, account *Account, modelID, prompt string) (string, error) {
	internalModel := resolveKiroInternalModel(account, modelID)
	contentRaw, _ := json.Marshal(prompt)
	req := &kiroOpenAIRequest{
		Model:  modelID,
		Stream: true,
		Messages: []kiroOpenAIMessage{
			{Role: "user", Content: contentRaw},
		},
	}
	payload, err := buildKiroPayload(req, internalModel, account.KiroProfileArn())
	if err != nil {
		return "", fmt.Errorf("build payload: %w", err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	region := account.KiroRegion()
	if region == "" {
		region = KiroDefaultRegion
	}
	endpoint := fmt.Sprintf(kiroGenerateEndpointTmpl, region)

	probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(probeCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	machineID := account.KiroMachineID()
	if machineID == "" {
		machineID = "sub2api"
	}
	ua := fmt.Sprintf(KiroIDEUserAgentTmpl, machineID)
	httpReq.Header.Set("Authorization", "Bearer "+account.KiroAccessToken())
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/vnd.amazon.eventstream")
	httpReq.Header.Set("X-Amz-User-Agent", ua)
	httpReq.Header.Set("User-Agent", ua)
	httpReq.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	httpReq.Header.Set("amz-sdk-request", "attempt=1; max=3")
	httpReq.Header.Set("x-amzn-kiro-agent-mode", kiroAgentMode)
	if account.KiroProfileArn() != "" {
		httpReq.Header.Set("x-amzn-kiro-profile-arn", account.KiroProfileArn())
	}

	client, err := httpclient.GetClient(httpclient.Options{
		Timeout:            60 * time.Second,
		ValidateResolvedIP: true,
	})
	if err != nil {
		return "", err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var assembled strings.Builder
	if rerr := readKiroFrames(resp.Body, func(payload []byte) error {
		ev := extractKiroDelta(payload)
		if ev.Text != "" {
			_, _ = assembled.WriteString(ev.Text)
			if c != nil {
				s.sendEvent(c, TestEvent{Type: "content", Text: ev.Text})
			}
		}
		return nil
	}); rerr != nil {
		return "", rerr
	}
	return assembled.String(), nil
}

func kiroCappedLabel(a *Account) string {
	if a.KiroIsCapped() {
		return "capped"
	}
	return "available"
}

// persistKiroCredentials 仅更新 credentials 字段，保留其他字段不变。
func (s *AccountTestService) persistKiroCredentials(ctx context.Context, id int64, creds map[string]any) error {
	acc, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	acc.Credentials = creds
	return s.accountRepo.Update(ctx, acc)
}

func (s *AccountTestService) persistKiroExtra(ctx context.Context, id int64, extra map[string]any) error {
	acc, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	acc.Extra = extra
	return s.accountRepo.Update(ctx, acc)
}
