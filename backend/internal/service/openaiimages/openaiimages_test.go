package openaiimages

import (
	"strings"
	"testing"
)

func TestLookupCapability(t *testing.T) {
	cases := map[string]bool{
		"gpt-image-2":       true,
		"GPT-Image-2":       true,
		"  gpt-5-mini  ":    true,
		"codex-gpt-image-2": true,
		"dall-e-3":          true,
		"unknown-model":     false,
		"":                  false,
	}
	for in, want := range cases {
		if _, ok := LookupCapability(in); ok != want {
			t.Errorf("LookupCapability(%q) = %v, want %v", in, ok, want)
		}
	}
}

func TestResolveDriverName(t *testing.T) {
	webCap, _ := LookupCapability("gpt-image-2")
	apiCap, _ := LookupCapability("dall-e-3")

	if got := ResolveDriverName(apiCap, &fakeAccount{apiKey: true}); got != DriverAPIKey {
		t.Errorf("apikey account → %q want %q", got, DriverAPIKey)
	}
	if got := ResolveDriverName(webCap, &fakeAccount{legacyEnabled: true}); got != DriverWeb {
		t.Errorf("web cap + toggle on → %q want %q", got, DriverWeb)
	}
	if got := ResolveDriverName(webCap, &fakeAccount{legacyEnabled: false}); got != DriverResponses {
		t.Errorf("web cap + toggle off → %q want %q", got, DriverResponses)
	}
	if got := ResolveDriverName(apiCap, &fakeAccount{legacyEnabled: true}); got != DriverAPIKey {
		t.Errorf("dall-e cap forces apikey, got %q", got)
	}
}

func TestRenderMarkdown(t *testing.T) {
	items := []ImageItem{
		{B64JSON: "aGVsbG8=", MimeType: "image/png", RevisedPrompt: "a cat"},
		{URL: "https://cdn.example/x.png"},
	}
	out := RenderMarkdown("a cat", items)
	if !strings.Contains(out, "data:image/png;base64,aGVsbG8=") {
		t.Errorf("expect data URL embedded, got: %s", out)
	}
	if !strings.Contains(out, "https://cdn.example/x.png") {
		t.Errorf("expect remote URL, got: %s", out)
	}
	// revised prompt 与原 prompt 相同时应不渲染
	if strings.Contains(out, "Revised prompt") {
		t.Errorf("revised prompt should be suppressed when equal to original")
	}
}

func TestExtractDataURLs(t *testing.T) {
	md := "![a](data:image/png;base64,AAA) and ![b](data:image/jpeg;base64,BBB)"
	urls := ExtractDataURLs(md)
	if len(urls) != 2 {
		t.Fatalf("expect 2 URLs, got %d (%v)", len(urls), urls)
	}
}

func TestParseImagesGenerations(t *testing.T) {
	body := []byte(`{"model":"gpt-image-2","prompt":"a dog","n":2,"size":"1024x1024","response_format":"url"}`)
	req, err := ParseImagesGenerations(body)
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if req.Entry != EntryImagesGenerations || req.N != 2 || req.ResponseFormat != ResponseFormatURL {
		t.Errorf("unexpected: %+v", req)
	}
}

func TestParseImagesGenerationsRejectsEmptyPrompt(t *testing.T) {
	if _, err := ParseImagesGenerations([]byte(`{"model":"gpt-image-2","prompt":""}`)); err == nil {
		t.Error("expect error for empty prompt")
	}
}

func TestParseFromChatCompletionsTextOnly(t *testing.T) {
	body := []byte(`{"model":"gpt-image-2","messages":[
		{"role":"system","content":"you are an artist"},
		{"role":"user","content":"draw a tiger"}
	]}`)
	req, err := ParseFromChatCompletions(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if req.Prompt != "draw a tiger" || req.Entry != EntryChatCompletions || req.ResponseFormat != ResponseFormatMarkdown {
		t.Errorf("got %+v", req)
	}
}

func TestParseFromChatCompletionsWithImage(t *testing.T) {
	// data:image/png;base64,iVBORw0K... 是 1x1 PNG header
	body := []byte(`{"model":"gpt-image-2","messages":[
		{"role":"user","content":[
			{"type":"text","text":"add a hat"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}
		]}
	]}`)
	req, err := ParseFromChatCompletions(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if req.Prompt != "add a hat" || len(req.Images) != 1 || req.Images[0].ContentType != "image/png" {
		t.Errorf("got %+v / %+v", req, req.Images)
	}
}

func TestParseFromResponsesString(t *testing.T) {
	body := []byte(`{"model":"gpt-image-2","input":"a knight","instructions":"render in oil"}`)
	req, err := ParseFromResponses(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(req.Prompt, "render in oil") || !strings.Contains(req.Prompt, "a knight") {
		t.Errorf("instructions should prefix prompt: %q", req.Prompt)
	}
}

// fakeAccount 实现 AccountView 用于单测。
type fakeAccount struct {
	apiKey        bool
	legacyEnabled bool
}

func (f *fakeAccount) ID() int64                            { return 0 }
func (f *fakeAccount) AccessToken() string                  { return "" }
func (f *fakeAccount) ChatGPTAccountID() string             { return "" }
func (f *fakeAccount) UserAgent() string                    { return "" }
func (f *fakeAccount) DeviceID() string                     { return "" }
func (f *fakeAccount) SessionID() string                    { return "" }
func (f *fakeAccount) ProxyURL() string                     { return "" }
func (f *fakeAccount) IsAPIKey() bool                       { return f.apiKey }
func (f *fakeAccount) APIKey() string                       { return "" }
func (f *fakeAccount) LegacyImagesEnabled() bool            { return f.legacyEnabled }
func (f *fakeAccount) QuotaSnapshot() *AccountQuotaSnapshot { return nil }
