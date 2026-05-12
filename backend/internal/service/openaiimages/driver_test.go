package openaiimages

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubAccount is a test AccountView for both drivers.
type stubAccount struct {
	apiKey string
	access string
	proxy  string
}

func (a *stubAccount) ID() int64                            { return 1 }
func (a *stubAccount) AccessToken() string                  { return a.access }
func (a *stubAccount) ChatGPTAccountID() string             { return "" }
func (a *stubAccount) UserAgent() string                    { return "" }
func (a *stubAccount) DeviceID() string                     { return "" }
func (a *stubAccount) SessionID() string                    { return "" }
func (a *stubAccount) ProxyURL() string                     { return a.proxy }
func (a *stubAccount) IsAPIKey() bool                       { return a.apiKey != "" }
func (a *stubAccount) APIKey() string                       { return a.apiKey }
func (a *stubAccount) LegacyImagesEnabled() bool            { return false }
func (a *stubAccount) QuotaSnapshot() *AccountQuotaSnapshot { return nil }

func TestAPIKeyDriver_GenerationsHappy(t *testing.T) {
	mux := http.NewServeMux()
	var gotBody map[string]any
	mux.HandleFunc("/v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("authorization"); got != "Bearer sk-test" {
			t.Errorf("authorization=%q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": 1700000000,
			"model":   "gpt-image-1",
			"data": []any{
				map[string]any{"b64_json": "AAAA"},
				map[string]any{"b64_json": "BBBB", "revised_prompt": "rev"},
			},
			"usage": map[string]any{"input_tokens": 5, "output_tokens": 10, "total_tokens": 15},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := NewAPIKeyDriver()
	d.BaseURL = srv.URL
	res, err := d.Forward(context.Background(), &stubAccount{apiKey: "sk-test"}, &ImagesRequest{
		Entry:  EntryImagesGenerations,
		Model:  "gpt-image-2",
		Prompt: "a cat",
		N:      2,
		Size:   "1024x1024",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotBody["model"] != "gpt-image-1" {
		t.Errorf("upstream model alias not mapped: %v", gotBody["model"])
	}
	if gotBody["prompt"] != "a cat" {
		t.Errorf("prompt: %v", gotBody["prompt"])
	}
	if gotBody["size"] != "1024x1024" {
		t.Errorf("size: %v", gotBody["size"])
	}
	if n, _ := gotBody["n"].(float64); int(n) != 2 {
		t.Errorf("n: %v", gotBody["n"])
	}
	if gotBody["response_format"] != "b64_json" {
		t.Errorf("response_format: %v", gotBody["response_format"])
	}
	if len(res.Items) != 2 {
		t.Fatalf("expect 2 items, got %d", len(res.Items))
	}
	if res.Items[0].B64JSON != "AAAA" || res.Items[1].RevisedPrompt != "rev" {
		t.Errorf("items wrong: %+v", res.Items)
	}
	if res.Usage.TotalTokens != 15 || res.Usage.ImagesCount != 2 {
		t.Errorf("usage: %+v", res.Usage)
	}
}

func TestAPIKeyDriver_EditsMultipart(t *testing.T) {
	mux := http.NewServeMux()
	var (
		gotPrompt  string
		gotModel   string
		gotN       string
		imgFiles   []string
		imgContent map[string]string
	)
	imgContent = map[string]string{}
	mux.HandleFunc("/v1/images/edits", func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("parse multipart ct: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("part: %v", err)
			}
			data, _ := io.ReadAll(p)
			switch p.FormName() {
			case "model":
				gotModel = string(data)
			case "prompt":
				gotPrompt = string(data)
			case "n":
				gotN = string(data)
			case "image", "mask":
				imgFiles = append(imgFiles, p.FormName())
				imgContent[p.FormName()] = string(data)
			}
		}
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"ZZZZ"}],"model":"gpt-image-1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := NewAPIKeyDriver()
	d.BaseURL = srv.URL
	res, err := d.Forward(context.Background(), &stubAccount{apiKey: "sk-x"}, &ImagesRequest{
		Entry:  EntryImagesEdits,
		Model:  "gpt-image-2",
		Prompt: "make blue",
		N:      1,
		Images: []SourceImage{
			{Filename: "a.png", ContentType: "image/png", Data: []byte("RAW-IMG")},
			{Filename: "m.png", ContentType: "image/png", Data: []byte("RAW-MASK")},
		},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotModel != "gpt-image-1" || gotPrompt != "make blue" || gotN != "1" {
		t.Errorf("fields: model=%q prompt=%q n=%q", gotModel, gotPrompt, gotN)
	}
	if len(imgFiles) != 2 {
		t.Errorf("expect both image and mask uploaded, got %v", imgFiles)
	}
	if imgContent["image"] != "RAW-IMG" || imgContent["mask"] != "RAW-MASK" {
		t.Errorf("upload content wrong: %+v", imgContent)
	}
	if len(res.Items) != 1 || res.Items[0].B64JSON != "ZZZZ" {
		t.Errorf("items: %+v", res.Items)
	}
}

func TestAPIKeyDriver_RateLimitWith429(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"too many"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	d := NewAPIKeyDriver()
	d.BaseURL = srv.URL
	_, err := d.Forward(context.Background(), &stubAccount{apiKey: "sk"}, &ImagesRequest{Prompt: "x"})
	rl, ok := err.(*RateLimitError)
	if !ok {
		t.Fatalf("expect RateLimitError got %T %v", err, err)
	}
	if rl.ResetAfter != 120*time.Second {
		t.Errorf("ResetAfter=%v", rl.ResetAfter)
	}
	if rl.Reason != "too many" {
		t.Errorf("reason=%q", rl.Reason)
	}
}

func TestAPIKeyDriver_AuthAndUpstreamError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		check  func(t *testing.T, err error)
	}{
		{"401 → AuthError", 401, func(t *testing.T, err error) {
			if !IsAuth(err) {
				t.Errorf("expect AuthError got %T", err)
			}
		}},
		{"403 → AuthError", 403, func(t *testing.T, err error) {
			if !IsAuth(err) {
				t.Errorf("expect AuthError got %T", err)
			}
		}},
		{"500 → TransportError", 500, func(t *testing.T, err error) {
			var tr *TransportError
			if !asAs(err, &tr) {
				t.Errorf("expect TransportError got %T", err)
			}
		}},
		{"400 → UpstreamError", 400, func(t *testing.T, err error) {
			var ue *UpstreamError
			if !asAs(err, &ue) {
				t.Errorf("expect UpstreamError got %T", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"bad"}}`))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			d := NewAPIKeyDriver()
			d.BaseURL = srv.URL
			_, err := d.Forward(context.Background(), &stubAccount{apiKey: "sk"}, &ImagesRequest{Prompt: "x"})
			tc.check(t, err)
		})
	}
}

func TestAPIKeyDriver_MissingKeyAuthError(t *testing.T) {
	d := NewAPIKeyDriver()
	_, err := d.Forward(context.Background(), &stubAccount{}, &ImagesRequest{Prompt: "x"})
	if !IsAuth(err) {
		t.Errorf("expect AuthError, got %v", err)
	}
}

func TestUpstreamModelMapping(t *testing.T) {
	cases := map[string]string{
		"":                  "gpt-image-1",
		"auto":              "gpt-image-1",
		"gpt-image-2":       "gpt-image-1",
		"codex-gpt-image-2": "gpt-image-1",
		"dall-e-3":          "dall-e-3",
		"my-custom":         "my-custom",
	}
	for in, want := range cases {
		if got := upstreamModel(in); got != want {
			t.Errorf("upstreamModel(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if parseRetryAfter("60") != 60*time.Second {
		t.Error("seconds parse failed")
	}
	if parseRetryAfter("") != 0 {
		t.Error("empty should be 0")
	}
	future := time.Now().UTC().Add(45 * time.Second).Format(http.TimeFormat)
	d := parseRetryAfter(future)
	if d <= 0 || d > 60*time.Second {
		t.Errorf("HTTP date parse: got %v", d)
	}
	if parseRetryAfter("garbage") != 0 {
		t.Error("garbage should be 0")
	}
}

// --- Responses-Tool driver ---

func TestResponsesToolDriver_HappyTextOnly(t *testing.T) {
	mux := http.NewServeMux()
	var gotBody map[string]any
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("authorization missing")
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":      "gpt-image-1",
			"created_at": 1700000001,
			"output": []any{
				map[string]any{
					"type":           "image_generation_call",
					"result":         "AAAA",
					"revised_prompt": "rev",
				},
			},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 2, "total_tokens": 3},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := NewResponsesToolDriver()
	d.BaseURL = srv.URL

	res, err := d.Forward(context.Background(), &stubAccount{apiKey: "sk-tok"}, &ImagesRequest{
		Entry:  EntryImagesGenerations,
		Model:  "gpt-image-2",
		Prompt: "a tiger",
		Size:   "512x512",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotBody["input"] != "a tiger" {
		t.Errorf("input=%v want string prompt", gotBody["input"])
	}
	tools, _ := gotBody["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "image_generation" || tool["size"] != "512x512" {
		t.Errorf("tool spec wrong: %v", tool)
	}
	if len(res.Items) != 1 || res.Items[0].B64JSON != "AAAA" || res.Items[0].RevisedPrompt != "rev" {
		t.Errorf("items: %+v", res.Items)
	}
	if res.Usage.TotalTokens != 3 {
		t.Errorf("usage: %+v", res.Usage)
	}
}

func TestResponsesToolDriver_EditsBuildsImageInput(t *testing.T) {
	mux := http.NewServeMux()
	var gotBody map[string]any
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": []any{
				map[string]any{"type": "image_generation_call", "result": "ZZZ"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := NewResponsesToolDriver()
	d.BaseURL = srv.URL

	_, err := d.Forward(context.Background(), &stubAccount{apiKey: "sk-tok"}, &ImagesRequest{
		Entry:  EntryImagesEdits,
		Model:  "auto",
		Prompt: "make blue",
		Images: []SourceImage{{ContentType: "image/png", Data: []byte("RAWPNG")}},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	inp, ok := gotBody["input"].([]any)
	if !ok || len(inp) != 1 {
		t.Fatalf("input shape wrong: %T %v", gotBody["input"], gotBody["input"])
	}
	msg, _ := inp[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("role=%v", msg["role"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content len=%d", len(content))
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "input_text" {
		t.Error("first should be input_text")
	}
	imgItem, _ := content[1].(map[string]any)
	if imgItem["type"] != "input_image" {
		t.Error("second should be input_image")
	}
	url, _ := imgItem["image_url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("image_url=%q", url)
	}
}

func TestResponsesToolDriver_NoImageOutputErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"refused"}]}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	d := NewResponsesToolDriver()
	d.BaseURL = srv.URL
	_, err := d.Forward(context.Background(), &stubAccount{apiKey: "sk-tok"}, &ImagesRequest{Prompt: "x"})
	var ue *UpstreamError
	if !asAs(err, &ue) {
		t.Errorf("expect UpstreamError, got %T %v", err, err)
	}
}

func TestResponsesToolDriver_AuthError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid token"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	d := NewResponsesToolDriver()
	d.BaseURL = srv.URL
	_, err := d.Forward(context.Background(), &stubAccount{apiKey: "sk-tok"}, &ImagesRequest{Prompt: "x"})
	if !IsAuth(err) {
		t.Errorf("expect Auth err, got %T %v", err, err)
	}
}

// asAs: errors.As shorthand for tests using *T pointer to pointer.
func asAs(err error, target any) bool {
	// errors.As needs a pointer to an interface or pointer-to-pointer.
	switch v := target.(type) {
	case **TransportError:
		var x *TransportError
		if e, ok := err.(*TransportError); ok {
			x = e
			*v = x
			return true
		}
	case **UpstreamError:
		var x *UpstreamError
		if e, ok := err.(*UpstreamError); ok {
			x = e
			*v = x
			return true
		}
	case **AuthError:
		var x *AuthError
		if e, ok := err.(*AuthError); ok {
			x = e
			*v = x
			return true
		}
	}
	return false
}

// --- OAuth path (chatgpt.com codex /backend-api/codex/responses + SSE) ---

// stubOAuthAccount is OAuth-typed (no apiKey, has accessToken + chatGPTAcctID + UA).
type stubOAuthAccount struct {
access string
acctID string
ua     string
sess   string
}

func (a *stubOAuthAccount) ID() int64                            { return 42 }
func (a *stubOAuthAccount) AccessToken() string                  { return a.access }
func (a *stubOAuthAccount) ChatGPTAccountID() string             { return a.acctID }
func (a *stubOAuthAccount) UserAgent() string                    { return a.ua }
func (a *stubOAuthAccount) DeviceID() string                     { return "" }
func (a *stubOAuthAccount) SessionID() string                    { return a.sess }
func (a *stubOAuthAccount) ProxyURL() string                     { return "" }
func (a *stubOAuthAccount) IsAPIKey() bool                       { return false }
func (a *stubOAuthAccount) APIKey() string                       { return "" }
func (a *stubOAuthAccount) LegacyImagesEnabled() bool            { return false }
func (a *stubOAuthAccount) QuotaSnapshot() *AccountQuotaSnapshot { return nil }

func TestResponsesToolDriver_OAuthRoutesToCodexEndpointAndParsesSSE(t *testing.T) {
mux := http.NewServeMux()
var (
gotPath        string
gotBody        map[string]any
gotAuth        string
gotChatAcctID  string
gotBeta        string
gotOriginator  string
gotAccept      string
gotSessionID   string
gotUserAgent   string
)

mux.HandleFunc("/backend-api/codex/responses", func(w http.ResponseWriter, r *http.Request) {
gotPath = r.URL.Path
gotAuth = r.Header.Get("authorization")
gotChatAcctID = r.Header.Get("chatgpt-account-id")
gotBeta = r.Header.Get("openai-beta")
gotOriginator = r.Header.Get("originator")
gotAccept = r.Header.Get("accept")
gotSessionID = r.Header.Get("session_id")
gotUserAgent = r.Header.Get("user-agent")
_ = json.NewDecoder(r.Body).Decode(&gotBody)

w.Header().Set("Content-Type", "text/event-stream")
w.WriteHeader(200)
// 模拟上游 Codex SSE：lifecycle + output_item.done + completed
_, _ = io.WriteString(w, "event: response.created\n")
_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"created_at":1700000999,"tools":[{"model":"gpt-image-1"}]}}`+"\n\n")
_, _ = io.WriteString(w, "event: response.output_item.done\n")
_, _ = io.WriteString(w, `data: {"type":"response.output_item.done","item":{"id":"img_abc","type":"image_generation_call","result":"AAAA","revised_prompt":"rev"}}`+"\n\n")
_, _ = io.WriteString(w, "event: response.completed\n")
_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"created_at":1700001000,"output":[{"id":"img_abc","type":"image_generation_call","result":"AAAA","revised_prompt":"rev"}],"usage":{"input_tokens":3,"output_tokens":7,"total_tokens":10}}}`+"\n\n")
})
srv := httptest.NewServer(mux)
defer srv.Close()

d := NewResponsesToolDriver()
d.OAuthBaseURL = srv.URL // 测试时把 codex 端点指到 httptest

res, err := d.Forward(context.Background(), &stubOAuthAccount{
access: "oauth-tok-xyz",
acctID: "acct-77",
ua:     "MyCodexUA/1.0",
}, &ImagesRequest{
Entry:  EntryImagesGenerations,
Model:  "gpt-image-2",
Prompt: "a cyberpunk city at night",
Size:   "1792x1024",
})
if err != nil {
t.Fatalf("err: %v", err)
}

// Endpoint
if gotPath != "/backend-api/codex/responses" {
t.Errorf("path=%q want /backend-api/codex/responses", gotPath)
}
// Auth + headers
if gotAuth != "Bearer oauth-tok-xyz" {
t.Errorf("authorization=%q", gotAuth)
}
if gotChatAcctID != "acct-77" {
t.Errorf("chatgpt-account-id=%q", gotChatAcctID)
}
if gotBeta != "responses=experimental" {
t.Errorf("openai-beta=%q want responses=experimental", gotBeta)
}
if gotOriginator != "codex_cli_rs" {
t.Errorf("originator=%q", gotOriginator)
}
if gotAccept != "text/event-stream" {
t.Errorf("accept=%q", gotAccept)
}
if gotSessionID == "" {
t.Errorf("session_id should be set")
}
if gotUserAgent != "MyCodexUA/1.0" {
t.Errorf("user-agent=%q want passthrough from account", gotUserAgent)
}

// Body shape: 双层 model + stream:true + reasoning + tool.action + include
if gotBody["model"] != "gpt-5.4-mini" {
t.Errorf("top-level model=%v want gpt-5.4-mini (main model)", gotBody["model"])
}
if gotBody["stream"] != true {
t.Errorf("stream=%v want true", gotBody["stream"])
}
if gotBody["store"] != false {
t.Errorf("store=%v want false", gotBody["store"])
}
if gotBody["parallel_tool_calls"] != true {
t.Errorf("parallel_tool_calls=%v want true", gotBody["parallel_tool_calls"])
}
include, _ := gotBody["include"].([]any)
if len(include) == 0 || include[0] != "reasoning.encrypted_content" {
t.Errorf("include=%v want [reasoning.encrypted_content]", include)
}
reasoning, _ := gotBody["reasoning"].(map[string]any)
if reasoning["effort"] != "medium" || reasoning["summary"] != "auto" {
t.Errorf("reasoning=%v", reasoning)
}
tools, _ := gotBody["tools"].([]any)
tool, _ := tools[0].(map[string]any)
if tool["type"] != "image_generation" || tool["action"] != "generate" || tool["model"] != "gpt-image-1" {
t.Errorf("tool=%v want type=image_generation action=generate model=gpt-image-1", tool)
}
if tool["size"] != "1792x1024" {
t.Errorf("tool.size=%v", tool["size"])
}
// input must be array of message
inp, _ := gotBody["input"].([]any)
if len(inp) != 1 {
t.Fatalf("input shape: %v", inp)
}
msg, _ := inp[0].(map[string]any)
if msg["type"] != "message" || msg["role"] != "user" {
t.Errorf("input[0]=%v", msg)
}

// Result
if len(res.Items) != 1 || res.Items[0].B64JSON != "AAAA" || res.Items[0].RevisedPrompt != "rev" {
t.Errorf("items: %+v", res.Items)
}
if res.Created != 1700001000 {
t.Errorf("created=%d", res.Created)
}
if res.Usage.TotalTokens != 10 || res.Usage.InputTokens != 3 || res.Usage.OutputTokens != 7 {
t.Errorf("usage: %+v", res.Usage)
}
if res.Model != "gpt-image-1" {
t.Errorf("model=%q want gpt-image-1 (from tools[0].model)", res.Model)
}
}

func TestResponsesToolDriver_OAuthEditsActionEdit(t *testing.T) {
mux := http.NewServeMux()
var gotBody map[string]any
mux.HandleFunc("/backend-api/codex/responses", func(w http.ResponseWriter, r *http.Request) {
_ = json.NewDecoder(r.Body).Decode(&gotBody)
w.Header().Set("Content-Type", "text/event-stream")
_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"ZZZ"}]}}`+"\n\n")
})
srv := httptest.NewServer(mux)
defer srv.Close()

d := NewResponsesToolDriver()
d.OAuthBaseURL = srv.URL

_, err := d.Forward(context.Background(), &stubOAuthAccount{access: "tok"}, &ImagesRequest{
Entry:  EntryImagesEdits,
Model:  "auto",
Prompt: "make it red",
Images: []SourceImage{{ContentType: "image/png", Data: []byte("RAW")}},
})
if err != nil {
t.Fatalf("err: %v", err)
}
tools, _ := gotBody["tools"].([]any)
tool, _ := tools[0].(map[string]any)
if tool["action"] != "edit" {
t.Errorf("tool.action=%v want edit (Entry=Edits)", tool["action"])
}
}

func TestResponsesToolDriver_OAuthAuthErrorReturnsAuthError(t *testing.T) {
mux := http.NewServeMux()
mux.HandleFunc("/backend-api/codex/responses", func(w http.ResponseWriter, r *http.Request) {
w.WriteHeader(401)
_, _ = w.Write([]byte(`{"error":{"message":"You have insufficient permissions","code":"missing_scope"}}`))
})
srv := httptest.NewServer(mux)
defer srv.Close()
d := NewResponsesToolDriver()
d.OAuthBaseURL = srv.URL

_, err := d.Forward(context.Background(), &stubOAuthAccount{access: "tok"}, &ImagesRequest{Prompt: "x"})
var ae *AuthError
if !asAs(err, &ae) {
t.Fatalf("want AuthError, got %T %v", err, err)
}
if ae.HTTPStatus != 401 {
t.Errorf("status=%d", ae.HTTPStatus)
}
}

func TestResponsesToolDriver_APIKeyStillUsesPlatformURL(t *testing.T) {
mux := http.NewServeMux()
var gotPath string
mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
gotPath = r.URL.Path
_ = json.NewEncoder(w).Encode(map[string]any{
"output": []any{map[string]any{"type": "image_generation_call", "result": "OK"}},
})
})
srv := httptest.NewServer(mux)
defer srv.Close()
d := NewResponsesToolDriver()
d.BaseURL = srv.URL
d.OAuthBaseURL = "http://should-not-be-used.invalid" // 保证 APIKey 不会误走 OAuth 路径

_, err := d.Forward(context.Background(), &stubAccount{apiKey: "sk-test"}, &ImagesRequest{Prompt: "x"})
if err != nil {
t.Fatalf("err: %v", err)
}
if gotPath != "/v1/responses" {
t.Errorf("APIKey path went to %q, want /v1/responses", gotPath)
}
}
