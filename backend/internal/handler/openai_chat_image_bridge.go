package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Chat image aliases enter Images before user/process/account admission, so the
// existing limits and accounting cover every protocol using exactly one slot.
func (h *OpenAIGatewayHandler) chatImageBridge(c *gin.Context, body []byte) {
	converted, err := chatImageRequest(body)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	oldPath, oldBody, oldWriter := c.Request.URL.Path, c.Request.Body, c.Writer
	writer := &chatImageWriter{ResponseWriter: oldWriter, header: make(http.Header), stream: gjson.GetBytes(body, "stream").Bool()}
	c.Writer = writer
	c.Request.URL.Path = "/v1/images/generations"
	if len(gjson.GetBytes(converted, "images").Array()) > 0 {
		c.Request.URL.Path = "/v1/images/edits"
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(converted))
	c.Request.ContentLength = int64(len(converted))
	c.Request.Header.Set("Content-Type", "application/json")
	defer func() { c.Request.URL.Path = oldPath; c.Request.Body = oldBody; c.Writer = oldWriter }()
	h.Images(c)
	writer.finish(gjson.GetBytes(body, "model").String(), gjson.GetBytes(body, "stream").Bool())
}

func chatImageRequest(body []byte) ([]byte, error) {
	stream := gjson.GetBytes(body, "stream")
	if stream.Exists() && stream.Type != gjson.True && stream.Type != gjson.False {
		return nil, fmt.Errorf("stream must be a boolean")
	}
	var prompts []string
	var images []map[string]string
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return nil, fmt.Errorf("messages must be an array")
	}
	for _, message := range messages.Array() {
		role := message.Get("role").String()
		if role != "user" && role != "system" && role != "developer" {
			continue
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			prompts = append(prompts, content.String())
			continue
		}
		if !content.IsArray() {
			return nil, fmt.Errorf("unsupported image message content")
		}
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "text":
				prompts = append(prompts, part.Get("text").String())
			case "image_url":
				url := part.Get("image_url.url").String()
				if url == "" {
					return nil, fmt.Errorf("image_url.url is required")
				}
				images = append(images, map[string]string{"image_url": url})
			default:
				return nil, fmt.Errorf("unsupported image message content type")
			}
		}
	}
	prompt := strings.TrimSpace(strings.Join(prompts, "\n\n"))
	if prompt == "" {
		return nil, fmt.Errorf("image prompt is required")
	}
	payload := map[string]any{"model": gjson.GetBytes(body, "model").String(), "prompt": prompt, "stream": false, "response_format": "b64_json"}
	for _, key := range []string{"n", "size", "quality"} {
		if value := gjson.GetBytes(body, key); value.Exists() {
			payload[key] = value.Value()
		}
	}
	if len(images) > 0 {
		payload["images"] = images
	}
	return json.Marshal(payload)
}

type chatImageWriter struct {
	gin.ResponseWriter
	header http.Header
	body   bytes.Buffer
	status int
	stream bool
}

func (w *chatImageWriter) Header() http.Header { return w.header }
func (w *chatImageWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *chatImageWriter) WriteHeaderNow() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}
func (w *chatImageWriter) Write(data []byte) (int, error) {
	w.WriteHeaderNow()
	if len(bytes.TrimSpace(data)) == 0 && w.body.Len() == 0 {
		if w.stream {
			w.ResponseWriter.Header().Set("Content-Type", "text/event-stream")
			_, err := w.ResponseWriter.Write([]byte(": keepalive\n\n"))
			return len(data), err
		}
		w.ResponseWriter.Header().Set("Content-Type", "application/json")
		return w.ResponseWriter.Write(data)
	}
	return w.body.Write(data)
}
func (w *chatImageWriter) WriteString(data string) (int, error) { return w.Write([]byte(data)) }
func (w *chatImageWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
func (w *chatImageWriter) Size() int {
	if w.status == 0 {
		return -1
	}
	return w.body.Len()
}
func (w *chatImageWriter) Written() bool { return w.status != 0 }
func (w *chatImageWriter) Flush() {
	w.WriteHeaderNow()
	if w.body.Len() == 0 {
		w.ResponseWriter.Flush()
	}
}
func (w *chatImageWriter) finish(model string, stream bool) {
	output := w.ResponseWriter
	for key, values := range w.header {
		for _, value := range values {
			output.Header().Add(key, value)
		}
	}
	raw := w.body.Bytes()
	items := gjson.GetBytes(raw, "data")
	if w.Status() >= 400 || !items.IsArray() {
		if stream && output.Written() {
			_, _ = fmt.Fprintf(output, "data: %s\n\ndata: [DONE]\n\n", raw)
			output.Flush()
			return
		}
		output.WriteHeader(w.Status())
		_, _ = output.Write(raw)
		return
	}
	var content strings.Builder
	for _, item := range items.Array() {
		url := item.Get("url").String()
		if encoded := item.Get("b64_json").String(); encoded != "" {
			url = "data:image/png;base64," + encoded
		}
		if url != "" {
			fmt.Fprintf(&content, "![image](%s)\n", url)
		}
	}
	created := time.Now().Unix()
	id := fmt.Sprintf("chatcmpl-image-%d", time.Now().UnixNano())
	if !stream {
		output.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(gin.H{"id": id, "object": "chat.completion", "created": created, "model": model, "choices": []any{gin.H{"index": 0, "message": gin.H{"role": "assistant", "content": content.String()}, "finish_reason": "stop"}}})
		output.WriteHeader(http.StatusOK)
		_, _ = output.Write(payload)
		return
	}
	output.Header().Set("Content-Type", "text/event-stream")
	output.WriteHeader(http.StatusOK)
	for _, choice := range []any{gin.H{"index": 0, "delta": gin.H{"role": "assistant", "content": content.String()}, "finish_reason": nil}, gin.H{"index": 0, "delta": gin.H{}, "finish_reason": "stop"}} {
		payload, _ := json.Marshal(gin.H{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{choice}})
		_, _ = fmt.Fprintf(output, "data: %s\n\n", payload)
	}
	_, _ = fmt.Fprint(output, "data: [DONE]\n\n")
	output.Flush()
}
