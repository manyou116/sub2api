package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func (s *AccountTestService) testOpenAIConnectionProtocol(ctx context.Context, c *gin.Context, account *Account, model, prompt, mode string) error {
	if mode == AccountTestModeCompact || isOpenAIImageModel(model) {
		return s.sendErrorAndEnd(c, "The selected OpenAI authorization connection does not support this test")
	}
	if s.openaiGatewayService == nil {
		return s.sendErrorAndEnd(c, "OpenAI gateway is unavailable")
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "hi"
	}
	body, err := json.Marshal(map[string]any{"model": model, "input": prompt, "stream": false})
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to prepare OpenAI connection test")
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})

	// Invoke the production connection route and translate its terminal response
	// into the account test event protocol shared by browser and scheduled tests.
	recorder := httptest.NewRecorder()
	gatewayContext, _ := gin.CreateTestContext(recorder)
	gatewayContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	result, err := s.openaiGatewayService.Forward(ctx, gatewayContext, account, body)
	if err != nil || result == nil || recorder.Code < 200 || recorder.Code >= 300 {
		return s.sendErrorAndEnd(c, "OpenAI authorization connection test failed")
	}
	for _, item := range gjson.GetBytes(recorder.Body.Bytes(), "output").Array() {
		if item.Get("type").String() != "message" {
			continue
		}
		for _, content := range item.Get("content").Array() {
			if text := content.Get("text").String(); text != "" {
				s.sendEvent(c, TestEvent{Type: "content", Text: text})
			}
		}
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}
