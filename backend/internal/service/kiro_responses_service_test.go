//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestMarshalKiroChatBodyFromCompat_MapsMaxCompletionTokens(t *testing.T) {
	max := 256
	body, err := marshalKiroChatBodyFromCompat(&apicompat.ChatCompletionsRequest{
		Model:               "claude-sonnet-4.5",
		MaxCompletionTokens: &max,
		Messages: []apicompat.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`"hi"`),
		}},
	})
	require.NoError(t, err)
	require.Equal(t, int64(256), gjson.GetBytes(body, "max_tokens").Int())
}

func TestKiroResponses_RejectsPreviousResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	svc := NewKiroChatService()
	account := &Account{Platform: PlatformKiro, Type: AccountTypeOAuth}
	body := []byte(`{"model":"claude-sonnet-4.5","input":"hi","previous_response_id":"resp_abc"}`)
	_, err := svc.Responses(context.Background(), c, account, body, "")
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "previous_response_id")
}

func TestKiroResponses_InvalidJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	svc := NewKiroChatService()
	account := &Account{Platform: PlatformKiro, Type: AccountTypeOAuth}
	_, err := svc.Responses(context.Background(), c, account, []byte(`{`), "")
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestResponsesToChatBridge_BasicInput(t *testing.T) {
	// Guard: apicompat conversion used by Kiro Responses remains available.
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"claude-sonnet-4.5",
		"input":"hello from responses",
		"stream":false,
		"max_output_tokens":128
	}`), &req))
	chat, err := apicompat.ResponsesToChatCompletionsRequest(&req)
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4.5", chat.Model)
	require.NotEmpty(t, chat.Messages)
	body, err := marshalKiroChatBodyFromCompat(chat)
	require.NoError(t, err)
	var kiroReq kiroOpenAIRequest
	require.NoError(t, json.Unmarshal(body, &kiroReq))
	require.Equal(t, "claude-sonnet-4.5", kiroReq.Model)
	require.NotEmpty(t, kiroReq.Messages)
}

func TestEmitKiroChatChunks_TextAndTools(t *testing.T) {
	frames := bytes.Join([][]byte{
		kiroTestEventStreamFrame(`{"content":"Hel"}`),
		kiroTestEventStreamFrame(`{"content":"lo"}`),
		kiroTestEventStreamFrame(`{"toolUseId":"t1","name":"lookup"}`),
		kiroTestEventStreamFrame(`{"toolUseId":"t1","input":"{\"q\":"}`),
		kiroTestEventStreamFrame(`{"toolUseId":"t1","input":"1}"}`),
	}, nil)

	result := &KiroChatResult{InputTokens: 3}
	var chunks []*apicompat.ChatCompletionsChunk
	err := emitKiroChatChunks(bytes.NewReader(frames), "claude-sonnet-4.5", time.Now(), result, func(chunk *apicompat.ChatCompletionsChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, "Hello", result.AssembledContent)
	require.NotNil(t, result.FirstTokenMs)

	var texts []string
	var toolNames []string
	var sawToolArgs bool
	var finish string
	for _, ch := range chunks {
		if ch.Usage != nil {
			require.Equal(t, 3, ch.Usage.PromptTokens)
		}
		for _, choice := range ch.Choices {
			if choice.Delta.Content != nil {
				texts = append(texts, *choice.Delta.Content)
			}
			for _, tc := range choice.Delta.ToolCalls {
				if tc.Function.Name != "" {
					toolNames = append(toolNames, tc.Function.Name)
				}
				if tc.Function.Arguments != "" {
					sawToolArgs = true
				}
			}
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
	}
	require.Equal(t, []string{"Hel", "lo"}, texts)
	require.Equal(t, []string{"lookup"}, toolNames)
	require.True(t, sawToolArgs)
	require.Equal(t, "tool_calls", finish)
}

func TestBuildKiroChatCompletionsResponse_NonStream(t *testing.T) {
	frames := bytes.Join([][]byte{
		kiroTestEventStreamFrame(`{"content":"hi there"}`),
	}, nil)
	result := &KiroChatResult{InputTokens: 2}
	resp, err := buildKiroChatCompletionsResponse(bytes.NewReader(frames), "claude-sonnet-4.5", time.Now(), result)
	require.NoError(t, err)
	require.Equal(t, "chat.completion", resp.Object)
	require.Equal(t, "stop", resp.Choices[0].FinishReason)
	require.Contains(t, string(resp.Choices[0].Message.Content), "hi there")
	require.NotNil(t, resp.Usage)
	require.Equal(t, 2, resp.Usage.PromptTokens)
}

func TestStreamKiroAsResponses_EmitsTerminalEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	frames := bytes.Join([][]byte{
		kiroTestEventStreamFrame(`{"content":"ok"}`),
	}, nil)
	result := &KiroChatResult{}
	svc := NewKiroChatService()
	err := svc.streamKiroAsResponses(c, bytes.NewReader(frames), "claude-sonnet-4.5", time.Now(), result, nil, false, nil)
	require.NoError(t, err)
	body := rec.Body.String()
	require.Contains(t, body, "response.output_text.delta")
	require.Contains(t, body, "response.completed")
	require.Contains(t, body, "data: [DONE]")
	require.Equal(t, "ok", result.AssembledContent)
}

// kiroTestEventStreamFrame builds a minimal AWS EventStream frame with empty headers.
func kiroTestEventStreamFrame(payload string) []byte {
	p := []byte(payload)
	totalLen := 12 + len(p) + 4
	buf := make([]byte, totalLen)
	binary.BigEndian.PutUint32(buf[0:4], uint32(totalLen))
	binary.BigEndian.PutUint32(buf[4:8], 0)
	binary.BigEndian.PutUint32(buf[8:12], crc32.ChecksumIEEE(buf[0:8]))
	copy(buf[12:], p)
	binary.BigEndian.PutUint32(buf[totalLen-4:], crc32.ChecksumIEEE(buf[:totalLen-4]))
	return buf
}
