package openaiimages

import (
"bufio"
"bytes"
"context"
"encoding/json"
"fmt"
"io"
"net/http"
"strings"
"time"

"github.com/imroc/req/v3"
"github.com/tidwall/gjson"
)

// ResponsesToolDriver 走 OpenAI /responses + image_generation tool 生图。
//
// 两条并行子路径：
//
//   - APIKey 账号:  POST https://api.openai.com/v1/responses (JSON 一次性)
//   - OAuth 账号:   POST https://chatgpt.com/backend-api/codex/responses
//                   (SSE 流式, 双层 model: 顶层 main model + tool.model 是图片模型)
//
// OAuth 路径完全对齐上游 Wei-Shaw/sub2api 的 forwardOpenAIImagesOAuth +
// buildOpenAIImagesResponsesRequest，否则 ChatGPT codex 端点会拒绝
// (Missing scopes: api.responses.write 等)。
type ResponsesToolDriver struct {
BaseURL      string // APIKey 用，默认 https://api.openai.com
OAuthBaseURL string // OAuth 用，默认 https://chatgpt.com
MainModel    string // OAuth 顶层 model 字段，默认 gpt-5.4-mini
Originator   string // OAuth originator 头，默认 codex_cli_rs
UserAgent    string // 可选，OAuth 路径默认 codex-cli UA
Client       *req.Client
Now          func() time.Time
}

const (
codexResponsesPath = "/backend-api/codex/responses"
defaultMainModel   = "gpt-5.4-mini"
defaultOriginator  = "codex_cli_rs"
defaultCodexUA     = "codex_cli_rs/0.50.0 (linux x86_64)"
)

func NewResponsesToolDriver() *ResponsesToolDriver {
return &ResponsesToolDriver{
BaseURL:      "https://api.openai.com",
OAuthBaseURL: "https://chatgpt.com",
MainModel:    defaultMainModel,
Originator:   defaultOriginator,
Client:       req.C().SetTimeout(240 * time.Second),
Now:          time.Now,
}
}

func (d *ResponsesToolDriver) Name() string { return "responses-tool" }

func (d *ResponsesToolDriver) baseURL() string {
if d.BaseURL != "" {
return d.BaseURL
}
return "https://api.openai.com"
}

func (d *ResponsesToolDriver) oauthBaseURL() string {
if d.OAuthBaseURL != "" {
return d.OAuthBaseURL
}
return "https://chatgpt.com"
}

func (d *ResponsesToolDriver) mainModel() string {
if d.MainModel != "" {
return d.MainModel
}
return defaultMainModel
}

func (d *ResponsesToolDriver) originator() string {
if d.Originator != "" {
return d.Originator
}
return defaultOriginator
}

func (d *ResponsesToolDriver) httpClient() *req.Client {
if d.Client != nil {
return d.Client
}
d.Client = req.C().SetTimeout(240 * time.Second)
return d.Client
}

func (d *ResponsesToolDriver) now() time.Time {
if d.Now != nil {
return d.Now()
}
return time.Now()
}

// Forward 实现 Driver。按账号类型分发到 APIKey / OAuth 子路径。
func (d *ResponsesToolDriver) Forward(ctx context.Context, account AccountView, request *ImagesRequest) (*ImageResult, error) {
if account.IsAPIKey() {
return d.forwardAPIKey(ctx, account, request)
}
return d.forwardOAuth(ctx, account, request)
}

// --- APIKey path（兼容 sk-proj 直连 platform） ---

func (d *ResponsesToolDriver) forwardAPIKey(ctx context.Context, account AccountView, request *ImagesRequest) (*ImageResult, error) {
token := account.APIKey()
if token == "" {
token = account.AccessToken()
}
if token == "" {
return nil, &AuthError{Reason: "missing api key"}
}

client := d.httpClient()
if proxy := account.ProxyURL(); proxy != "" {
client = client.Clone().SetProxyURL(proxy)
}

body := d.buildBodyAPIKey(request)
resp, err := client.R().SetContext(ctx).
SetHeader("authorization", "Bearer "+token).
SetHeader("content-type", "application/json").
SetHeader("openai-beta", "responses=v1").
SetBodyJsonMarshal(body).
Post(d.baseURL() + "/v1/responses")
if err != nil {
return nil, &TransportError{Reason: err.Error()}
}
return d.parseResponseJSON(resp, request)
}

func (d *ResponsesToolDriver) buildBodyAPIKey(request *ImagesRequest) map[string]any {
tool := map[string]any{"type": "image_generation"}
if request.Size != "" {
tool["size"] = request.Size
}
if request.Quality != "" {
tool["quality"] = request.Quality
}
if request.Background != "" {
tool["background"] = request.Background
}

body := map[string]any{
"model":       upstreamModel(request.Model),
"input":       d.buildInput(request),
"tools":       []any{tool},
"tool_choice": map[string]any{"type": "image_generation"},
"stream":      false,
}
if request.User != "" {
body["user"] = request.User
}
for k, v := range request.Extras {
if _, exists := body[k]; !exists {
body[k] = v
}
}
return body
}

func (d *ResponsesToolDriver) buildInput(request *ImagesRequest) any {
if len(request.Images) == 0 {
return request.Prompt
}
content := []any{
map[string]any{"type": "input_text", "text": request.Prompt},
}
for _, img := range request.Images {
mime := img.ContentType
if mime == "" {
mime = "image/png"
}
content = append(content, map[string]any{
"type":      "input_image",
"image_url": fmt.Sprintf("data:%s;base64,%s", mime, encodeBase64(img.Data)),
})
}
return []any{
map[string]any{
"role":    "user",
"content": content,
},
}
}

// --- OAuth path（chatgpt.com codex 端点 + SSE） ---

func (d *ResponsesToolDriver) forwardOAuth(ctx context.Context, account AccountView, request *ImagesRequest) (*ImageResult, error) {
token := account.AccessToken()
if token == "" {
return nil, &AuthError{Reason: "missing access token"}
}

client := d.httpClient()
if proxy := account.ProxyURL(); proxy != "" {
client = client.Clone().SetProxyURL(proxy)
}

body := d.buildBodyOAuth(request)
bodyBytes, err := json.Marshal(body)
if err != nil {
return nil, &TransportError{Reason: "marshal body: " + err.Error()}
}

r := client.R().SetContext(ctx).
SetHeader("authorization", "Bearer "+token).
SetHeader("content-type", "application/json").
SetHeader("accept", "text/event-stream").
SetHeader("openai-beta", "responses=experimental").
SetHeader("originator", d.originator()).
SetHeader("session_id", isolatedSessionID(account)).
SetBodyBytes(bodyBytes)

if acctID := strings.TrimSpace(account.ChatGPTAccountID()); acctID != "" {
r = r.SetHeader("chatgpt-account-id", acctID)
}
if ua := strings.TrimSpace(account.UserAgent()); ua != "" {
r = r.SetHeader("user-agent", ua)
} else if d.UserAgent != "" {
r = r.SetHeader("user-agent", d.UserAgent)
} else {
r = r.SetHeader("user-agent", defaultCodexUA)
}

resp, err := r.Post(d.oauthBaseURL() + codexResponsesPath)
if err != nil {
return nil, &TransportError{Reason: err.Error()}
}
return d.parseResponseSSE(resp, request)
}

func (d *ResponsesToolDriver) buildBodyOAuth(request *ImagesRequest) map[string]any {
action := "generate"
if request.Entry == EntryImagesEdits || len(request.Images) > 0 {
action = "edit"
}

tool := map[string]any{
"type":   "image_generation",
"action": action,
"model":  upstreamModel(request.Model),
}
if request.Size != "" {
tool["size"] = request.Size
}
if request.Quality != "" {
tool["quality"] = request.Quality
}
if request.Background != "" {
tool["background"] = request.Background
}

body := map[string]any{
"model":               d.mainModel(),
"instructions":        "",
"stream":              true,
"store":               false,
"parallel_tool_calls": true,
"include":             []string{"reasoning.encrypted_content"},
"reasoning": map[string]any{
"effort":  "medium",
"summary": "auto",
},
"tool_choice": map[string]any{"type": "image_generation"},
"input":       d.buildInputOAuth(request),
"tools":       []any{tool},
}
if request.User != "" {
body["user"] = request.User
}
for k, v := range request.Extras {
if _, exists := body[k]; !exists {
body[k] = v
}
}
return body
}

// buildInputOAuth 强制 message 形态（即使无图也要 input_text 数组），上游 codex 端点要求。
func (d *ResponsesToolDriver) buildInputOAuth(request *ImagesRequest) any {
content := []any{
map[string]any{"type": "input_text", "text": request.Prompt},
}
for _, img := range request.Images {
mime := img.ContentType
if mime == "" {
mime = "image/png"
}
content = append(content, map[string]any{
"type":      "input_image",
"image_url": fmt.Sprintf("data:%s;base64,%s", mime, encodeBase64(img.Data)),
})
}
return []any{
map[string]any{
"type":    "message",
"role":    "user",
"content": content,
},
}
}

// parseResponseJSON 处理 APIKey 路径的一次性 JSON 响应。
func (d *ResponsesToolDriver) parseResponseJSON(resp *req.Response, request *ImagesRequest) (*ImageResult, error) {
if resp == nil {
return nil, &TransportError{Reason: "empty response"}
}
body := resp.Bytes()
if err := classifyHTTPError(resp.StatusCode, resp.Header.Get("Retry-After"), body); err != nil {
return nil, err
}

var payload struct {
Output []struct {
Type   string `json:"type"`
Result string `json:"result"`
Status string `json:"status"`
Error  *struct {
Message string `json:"message"`
Code    string `json:"code"`
} `json:"error"`
RevisedPrompt string `json:"revised_prompt"`
} `json:"output"`
Model     string `json:"model"`
CreatedAt int64  `json:"created_at"`
Usage     *struct {
InputTokens  int `json:"input_tokens"`
OutputTokens int `json:"output_tokens"`
TotalTokens  int `json:"total_tokens"`
} `json:"usage"`
Error *struct {
Message string `json:"message"`
Code    string `json:"code"`
} `json:"error"`
}
if err := json.Unmarshal(body, &payload); err != nil {
return nil, &UpstreamError{HTTPStatus: resp.StatusCode, Body: body, Reason: "unparseable JSON: " + err.Error()}
}
if payload.Error != nil && payload.Error.Message != "" {
return nil, &UpstreamError{HTTPStatus: resp.StatusCode, Body: body, Reason: payload.Error.Message}
}

out := &ImageResult{
Model:   coalesceStr(payload.Model, request.Model),
Created: coalesceInt64(payload.CreatedAt, d.now().Unix()),
}
for _, item := range payload.Output {
if !strings.EqualFold(item.Type, "image_generation_call") {
continue
}
if item.Error != nil && item.Error.Message != "" {
return nil, &UpstreamError{HTTPStatus: resp.StatusCode, Body: body, Reason: item.Error.Message}
}
if item.Result == "" {
continue
}
out.Items = append(out.Items, ImageItem{
B64JSON:       item.Result,
RevisedPrompt: item.RevisedPrompt,
MimeType:      "image/png",
})
}
if len(out.Items) == 0 {
return nil, &UpstreamError{HTTPStatus: resp.StatusCode, Body: body, Reason: "no image_generation_call output"}
}
if payload.Usage != nil {
out.Usage = Usage{
InputTokens:  payload.Usage.InputTokens,
OutputTokens: payload.Usage.OutputTokens,
TotalTokens:  payload.Usage.TotalTokens,
ImagesCount:  len(out.Items),
}
} else {
out.Usage = Usage{ImagesCount: len(out.Items)}
}
return out, nil
}

// parseResponseSSE 解析 OAuth 路径的 SSE 流，从 response.completed 或
// response.output_item.done 事件中收集 image_generation_call.result。
func (d *ResponsesToolDriver) parseResponseSSE(resp *req.Response, request *ImagesRequest) (*ImageResult, error) {
if resp == nil {
return nil, &TransportError{Reason: "empty response"}
}
if err := classifyHTTPErrorStream(resp); err != nil {
return nil, err
}

var (
items     []ImageItem
seen      = map[string]struct{}{}
createdAt int64
model     string
usage     Usage
)

collect := func(payload []byte) {
switch gjson.GetBytes(payload, "type").String() {
case "response.created", "response.in_progress", "response.completed":
if t := gjson.GetBytes(payload, "response.created_at").Int(); t > 0 {
createdAt = t
}
if m := strings.TrimSpace(gjson.GetBytes(payload, "response.tools.0.model").String()); m != "" {
model = m
}
if u := gjson.GetBytes(payload, "response.usage"); u.IsObject() {
usage.InputTokens = int(u.Get("input_tokens").Int())
usage.OutputTokens = int(u.Get("output_tokens").Int())
usage.TotalTokens = int(u.Get("total_tokens").Int())
}
}

switch gjson.GetBytes(payload, "type").String() {
case "response.output_item.done":
item := gjson.GetBytes(payload, "item")
if item.Get("type").String() != "image_generation_call" {
return
}
result := strings.TrimSpace(item.Get("result").String())
if result == "" {
return
}
id := strings.TrimSpace(item.Get("id").String())
key := id
if key == "" {
key = result
}
if _, ok := seen[key]; ok {
return
}
seen[key] = struct{}{}
items = append(items, ImageItem{
B64JSON:       result,
RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
MimeType:      "image/png",
})
case "response.completed":
output := gjson.GetBytes(payload, "response.output")
if !output.IsArray() {
return
}
for _, it := range output.Array() {
if it.Get("type").String() != "image_generation_call" {
continue
}
result := strings.TrimSpace(it.Get("result").String())
if result == "" {
continue
}
id := strings.TrimSpace(it.Get("id").String())
key := id
if key == "" {
key = result
}
if _, ok := seen[key]; ok {
continue
}
seen[key] = struct{}{}
items = append(items, ImageItem{
B64JSON:       result,
RevisedPrompt: strings.TrimSpace(it.Get("revised_prompt").String()),
MimeType:      "image/png",
})
}
}
}

if err := streamSSEPayloads(resp, collect); err != nil {
return nil, err
}

if len(items) == 0 {
body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
return nil, &UpstreamError{HTTPStatus: resp.StatusCode, Body: body, Reason: "no image_generation_call output in SSE"}
}
usage.ImagesCount = len(items)
return &ImageResult{
Items:   items,
Model:   coalesceStr(model, request.Model),
Created: coalesceInt64(createdAt, d.now().Unix()),
Usage:   usage,
}, nil
}

// classifyHTTPError 是 JSON 路径的状态码分类（错误时直接读 body）。
func classifyHTTPError(status int, retryAfter string, body []byte) error {
switch {
case status == http.StatusUnauthorized || status == http.StatusForbidden:
return &AuthError{Reason: extractAPIError(body), HTTPStatus: status}
case status == http.StatusTooManyRequests:
return &RateLimitError{
ResetAfter: parseRetryAfter(retryAfter),
Reason:     extractAPIError(body),
HTTPStatus: status,
}
case status >= 500:
return &TransportError{Reason: extractAPIError(body), HTTPStatus: status}
case status >= 400:
return &UpstreamError{HTTPStatus: status, Body: body}
}
return nil
}

// classifyHTTPErrorStream 是 SSE 路径的状态码分类（错误时主动读完 body 用于诊断）。
func classifyHTTPErrorStream(resp *req.Response) error {
status := resp.StatusCode
if status >= 200 && status < 300 {
return nil
}
body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
return classifyHTTPError(status, resp.Header.Get("Retry-After"), body)
}

// streamSSEPayloads 按行读取 resp.Body，把 `data:` 行累积到一个空行触发的批次，
// 把每个批次拼接后送给 fn。
func streamSSEPayloads(resp *req.Response, fn func([]byte)) error {
defer resp.Body.Close()
scanner := bufio.NewScanner(resp.Body)
scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

var lines [][]byte
flush := func() {
if len(lines) == 0 {
return
}
joined := bytes.Join(lines, []byte("\n"))
if gjson.ValidBytes(joined) {
fn(joined)
} else {
for _, ln := range lines {
if gjson.ValidBytes(ln) {
fn(ln)
}
}
}
lines = lines[:0]
}

for scanner.Scan() {
line := bytes.TrimRight(scanner.Bytes(), "\r")
if len(line) == 0 {
flush()
continue
}
if bytes.HasPrefix(line, []byte("data:")) {
data := bytes.TrimSpace(line[len("data:"):])
if string(data) == "[DONE]" {
continue
}
lines = append(lines, append([]byte(nil), data...))
}
}
flush()
if err := scanner.Err(); err != nil {
return &TransportError{Reason: "sse scan: " + err.Error()}
}
return nil
}

// isolatedSessionID 给 OAuth 请求派生一个稳定的 session_id（对齐上游 isolateOpenAISessionID 的语义）。
// 若账号已注入 SessionID 用之；否则用 StableUUIDForAccount。
func isolatedSessionID(account AccountView) string {
if s := strings.TrimSpace(account.SessionID()); s != "" {
return s
}
return StableUUIDForAccount(account.ID(), "responses")
}
