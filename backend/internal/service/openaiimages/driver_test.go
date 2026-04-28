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
	if got := int(gotBody["n"].(float64)); got != 2 {
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

	res, err := d.Forward(context.Background(), &stubAccount{access: "tok"}, &ImagesRequest{
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
	tools := gotBody["tools"].([]any)
	tool := tools[0].(map[string]any)
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

	_, err := d.Forward(context.Background(), &stubAccount{access: "tok"}, &ImagesRequest{
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
	msg := inp[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("role=%v", msg["role"])
	}
	content := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content len=%d", len(content))
	}
	if content[0].(map[string]any)["type"] != "input_text" {
		t.Error("first should be input_text")
	}
	imgItem := content[1].(map[string]any)
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
	_, err := d.Forward(context.Background(), &stubAccount{access: "tok"}, &ImagesRequest{Prompt: "x"})
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
	_, err := d.Forward(context.Background(), &stubAccount{access: "tok"}, &ImagesRequest{Prompt: "x"})
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
	}
	return false
}
