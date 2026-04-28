package openaiimages

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// memSink 是测试用的 ResponseSink；记录 status / headers / body / flush 次数。
type memSink struct {
	buf      bytes.Buffer
	headers  map[string]string
	status   int
	flushed  int
	statusOK bool
}

func newMemSink() *memSink { return &memSink{headers: map[string]string{}} }
func (m *memSink) Write(p []byte) (int, error)   { return m.buf.Write(p) }
func (m *memSink) SetHeader(k, v string)         { m.headers[k] = v }
func (m *memSink) WriteStatus(code int)          { m.status = code; m.statusOK = true }
func (m *memSink) Flush()                        { m.flushed++ }
func (m *memSink) String() string                { return m.buf.String() }
func (m *memSink) Bytes() []byte                 { return m.buf.Bytes() }

// parseSSEFrames 把 SSE 输出按双换行切分；返回 [{event, data, raw}] 顺序列表。
type sseFrame struct {
	Event string
	Data  string
}

func parseSSEFrames(s string) []sseFrame {
	chunks := strings.Split(strings.TrimRight(s, "\n"), "\n\n")
	var frames []sseFrame
	for _, ch := range chunks {
		if ch == "" {
			continue
		}
		var f sseFrame
		for _, line := range strings.Split(ch, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				f.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.Data = strings.TrimPrefix(line, "data: ")
			}
		}
		frames = append(frames, f)
	}
	return frames
}

func sampleResult() *ImageResult {
	return &ImageResult{
		Items: []ImageItem{
			{B64JSON: "AAAA", RevisedPrompt: "rev1"},
			{B64JSON: "BBBB"},
		},
		Model:   "gpt-image-1",
		Created: 1700000000,
		Usage:   Usage{InputTokens: 5, OutputTokens: 10, TotalTokens: 15, ImagesCount: 2},
	}
}

func sampleReq() *ImagesRequest {
	return &ImagesRequest{Entry: EntryImagesGenerations, Model: "gpt-image-2", Prompt: "a cat"}
}

func TestWriteStandard(t *testing.T) {
	sink := newMemSink()
	if err := WriteStandard(sink, sampleReq(), sampleResult(), WriteOptions{}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if sink.status != 200 || !strings.HasPrefix(sink.headers["Content-Type"], "application/json") {
		t.Fatalf("status=%d ct=%q", sink.status, sink.headers["Content-Type"])
	}
	var out struct {
		Created int64 `json:"created"`
		Data    []struct {
			B64JSON       string `json:"b64_json"`
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(sink.Bytes(), &out); err != nil {
		t.Fatalf("json: %v body=%s", err, sink)
	}
	if out.Created != 1700000000 || len(out.Data) != 2 {
		t.Errorf("body: %+v", out)
	}
	if out.Data[0].B64JSON != "AAAA" || out.Data[0].RevisedPrompt != "rev1" || out.Data[1].B64JSON != "BBBB" {
		t.Errorf("data: %+v", out.Data)
	}
	if out.Data[0].URL != "" {
		t.Error("url should be omitted when empty")
	}
}

func TestWriteStandard_URLAndBytes(t *testing.T) {
	res := &ImageResult{
		Items: []ImageItem{{URL: "https://x/y.png", B64JSON: "AAAA"}},
		Created: 1, Model: "m",
	}
	sink := newMemSink()
	if err := WriteStandard(sink, &ImagesRequest{}, res, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sink.String(), `"url":"https://x/y.png"`) {
		t.Errorf("missing url: %s", sink)
	}
}

func TestWriteChatSync(t *testing.T) {
	sink := newMemSink()
	err := WriteChatSync(sink, sampleReq(), sampleResult(),
		WriteOptions{ResponseID: "chatcmpl-fixed", ClientModel: "gpt-image-2"})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(sink.Bytes(), &out); err != nil {
		t.Fatalf("json: %v body=%s", err, sink)
	}
	if out["id"] != "chatcmpl-fixed" || out["object"] != "chat.completion" || out["model"] != "gpt-image-2" {
		t.Errorf("envelope: %+v", out)
	}
	choices := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices len=%d", len(choices))
	}
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	content := msg["content"].(string)
	if !strings.Contains(content, "data:image/png;base64,AAAA") {
		t.Errorf("missing first image in markdown content: %q", content)
	}
	if !strings.Contains(content, "data:image/png;base64,BBBB") {
		t.Errorf("missing second image: %q", content)
	}
	if !strings.Contains(content, "Revised prompt:* rev1") {
		t.Errorf("revised prompt missing: %q", content)
	}
	usage := out["usage"].(map[string]any)
	if int(usage["total_tokens"].(float64)) != 15 {
		t.Errorf("usage: %+v", usage)
	}
}

func TestWriteChatSSE(t *testing.T) {
	sink := newMemSink()
	err := WriteChatSSE(sink, sampleReq(), sampleResult(),
		WriteOptions{ResponseID: "chatcmpl-x", ClientModel: "gpt-image-2"})
	if err != nil {
		t.Fatal(err)
	}
	if sink.headers["Content-Type"] != "text/event-stream" {
		t.Errorf("ct=%q", sink.headers["Content-Type"])
	}
	if sink.flushed < 4 {
		t.Errorf("flushed=%d, expected >=4", sink.flushed)
	}
	frames := parseSSEFrames(sink.String())
	// role / content / finish / usage / [DONE]
	if len(frames) != 5 {
		t.Fatalf("frames=%d body=%s", len(frames), sink)
	}
	if frames[4].Data != "[DONE]" {
		t.Errorf("last frame should be [DONE], got %q", frames[4].Data)
	}
	// role frame
	var f0 map[string]any
	_ = json.Unmarshal([]byte(frames[0].Data), &f0)
	role := f0["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["role"]
	if role != "assistant" {
		t.Errorf("first frame must have role=assistant, got %v", role)
	}
	// content frame
	var f1 map[string]any
	_ = json.Unmarshal([]byte(frames[1].Data), &f1)
	content := f1["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"].(string)
	if !strings.Contains(content, "AAAA") {
		t.Errorf("content frame missing image: %q", content)
	}
	// finish frame
	var f2 map[string]any
	_ = json.Unmarshal([]byte(frames[2].Data), &f2)
	if f2["choices"].([]any)[0].(map[string]any)["finish_reason"] != "stop" {
		t.Error("third frame should have finish_reason=stop")
	}
	// usage frame
	var f3 map[string]any
	_ = json.Unmarshal([]byte(frames[3].Data), &f3)
	if _, ok := f3["usage"]; !ok {
		t.Error("4th frame should contain usage")
	}
}

func TestWriteResponsesSync(t *testing.T) {
	sink := newMemSink()
	err := WriteResponsesSync(sink, sampleReq(), sampleResult(),
		WriteOptions{ResponseID: "resp_x", ClientModel: "gpt-image-2"})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(sink.Bytes(), &out); err != nil {
		t.Fatalf("json: %v body=%s", err, sink)
	}
	if out["id"] != "resp_x" || out["status"] != "completed" || out["object"] != "response" {
		t.Errorf("envelope: %+v", out)
	}
	output := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len=%d", len(output))
	}
	msg := output[0].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("output[0]: %+v", msg)
	}
	contentArr := msg["content"].([]any)
	text := contentArr[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "AAAA") || !strings.Contains(text, "BBBB") {
		t.Errorf("text missing images: %q", text)
	}
	usage := out["usage"].(map[string]any)
	if int(usage["total_tokens"].(float64)) != 15 {
		t.Errorf("usage: %+v", usage)
	}
}

func TestWriteResponsesSSE(t *testing.T) {
	sink := newMemSink()
	err := WriteResponsesSSE(sink, sampleReq(), sampleResult(),
		WriteOptions{ResponseID: "resp_z", ClientModel: "gpt-image-2"})
	if err != nil {
		t.Fatal(err)
	}
	if sink.headers["Content-Type"] != "text/event-stream" {
		t.Errorf("ct=%q", sink.headers["Content-Type"])
	}
	frames := parseSSEFrames(sink.String())
	wantEvents := []string{
		"response.created",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
		"", // [DONE]
	}
	if len(frames) != len(wantEvents) {
		t.Fatalf("frames=%d want=%d body=%s", len(frames), len(wantEvents), sink)
	}
	for i, want := range wantEvents {
		if frames[i].Event != want {
			t.Errorf("frame[%d] event=%q want=%q", i, frames[i].Event, want)
		}
	}
	if frames[6].Data != "[DONE]" {
		t.Errorf("last frame should be [DONE], got %q", frames[6].Data)
	}

	// delta payload check
	var delta map[string]any
	_ = json.Unmarshal([]byte(frames[2].Data), &delta)
	if !strings.Contains(delta["delta"].(string), "AAAA") {
		t.Errorf("delta payload missing image: %v", delta["delta"])
	}
	if delta["item_id"] == nil || delta["item_id"] == "" {
		t.Errorf("delta missing item_id: %+v", delta)
	}

	// completed payload should include usage
	var completed map[string]any
	_ = json.Unmarshal([]byte(frames[5].Data), &completed)
	resp := completed["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Errorf("response.status=%v", resp["status"])
	}
	usage := resp["usage"].(map[string]any)
	if int(usage["total_tokens"].(float64)) != 15 {
		t.Errorf("usage: %+v", usage)
	}
}

func TestNonEmptyAndCreatedFallback(t *testing.T) {
	if v := nonEmpty("", " ", "x", "y"); v != "x" {
		t.Errorf("nonEmpty=%q", v)
	}
	if normalizedCreated(&ImageResult{Created: 0}) <= 0 {
		t.Error("fallback should yield positive timestamp")
	}
	if normalizedCreated(&ImageResult{Created: 99}) != 99 {
		t.Error("explicit Created should be preserved")
	}
}

func TestRandomIDIsHexAndUnique(t *testing.T) {
	a, b := randomID(), randomID()
	if len(a) != 24 || a == b {
		t.Errorf("randomID weak: %q vs %q", a, b)
	}
}
