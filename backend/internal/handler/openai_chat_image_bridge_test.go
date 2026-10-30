//go:build unit

package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"net/http/httptest"
	"testing"
)

func TestWebImageChatRequestTranslation(t *testing.T) {
	body, err := chatImageRequest([]byte(`{"model":"gpt-image-2","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"draw this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aW1hZ2U="}}]}]}`))
	require.NoError(t, err)
	require.Equal(t, "draw this", gjson.GetBytes(body, "prompt").String())
	require.Equal(t, "data:image/png;base64,aW1hZ2U=", gjson.GetBytes(body, "images.0.image_url").String())
	require.False(t, gjson.GetBytes(body, "stream").Bool())
	_, err = chatImageRequest([]byte(`{"model":"gpt-image-2","messages":[{"role":"user","content":[{"type":"input_audio"}]}]}`))
	require.Error(t, err)
}
func TestWebImageChatResponseTranslation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		writer := &chatImageWriter{ResponseWriter: c.Writer, header: make(map[string][]string)}
		_, err := writer.Write([]byte(`{"created":7,"data":[{"b64_json":"aW1hZ2U="}]}`))
		require.NoError(t, err)
		writer.finish("gpt-image-2", stream)
		require.Contains(t, rec.Body.String(), "data:image/png;base64,aW1hZ2U=")
		if stream {
			require.Contains(t, rec.Body.String(), "chat.completion.chunk")
			require.Contains(t, rec.Body.String(), "[DONE]")
		} else {
			require.Equal(t, "chat.completion", gjson.Get(rec.Body.String(), "object").String())
		}
	}
}

func TestWebImageChatKeepaliveAndLateError(t *testing.T) {
	for _, stream := range []bool{false, true} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		writer := &chatImageWriter{ResponseWriter: c.Writer, header: make(map[string][]string), stream: stream}
		_, err := writer.Write([]byte(" \n"))
		require.NoError(t, err)
		writer.Flush()
		require.True(t, rec.Flushed)
		if stream {
			require.Contains(t, rec.Body.String(), ": keepalive")
		} else {
			require.Equal(t, " \n", rec.Body.String())
		}
		_, err = writer.Write([]byte(`{"error":{"message":"upstream failed"}}`))
		require.NoError(t, err)
		writer.finish("gpt-image-2", stream)
		require.Contains(t, rec.Body.String(), "upstream failed")
		if stream {
			require.Contains(t, rec.Body.String(), "[DONE]")
		}
	}
}
