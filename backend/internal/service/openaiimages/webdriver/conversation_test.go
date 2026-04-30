package webdriver

import (
	"strings"
	"testing"
)

func TestExtractSSEDataLine(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOk bool
	}{
		{"data: hello", "hello", true},
		{"data:{\"x\":1}", `{"x":1}`, true},
		{"event: ping", "", false},
		{"data: [DONE]", "[DONE]", true},
	}
	for _, c := range cases {
		got, ok := extractSSEDataLine(c.in)
		if got != c.want || ok != c.wantOk {
			t.Errorf("extractSSEDataLine(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOk)
		}
	}
}

func TestCollectPointers_FiltersExcluded(t *testing.T) {
	body := []byte(`"asset_pointer":"file-service://aaa","other":"sediment://bbb"`)
	excluded := map[string]struct{}{"file-service://aaa": {}}
	got := collectPointers(body, excluded)
	if len(got) != 1 || got[0].Pointer != "sediment://bbb" {
		t.Errorf("got %+v", got)
	}
}

func TestCountDownloadablePointers(t *testing.T) {
	items := []pointerInfo{
		{Pointer: "file-service://x"},
		{Pointer: "sediment://y"},
		{Pointer: "https://other"},
	}
	if n := countDownloadablePointers(items); n != 2 {
		t.Errorf("n = %d", n)
	}
}

func TestPreferFileService(t *testing.T) {
	in := []pointerInfo{{Pointer: "sediment://a"}, {Pointer: "file-service://b"}, {Pointer: "sediment://c"}}
	out := preferFileService(in)
	if len(out) != 1 || out[0].Pointer != "file-service://b" {
		t.Errorf("got %+v", out)
	}
}

func TestMergePointers_Dedup(t *testing.T) {
	a := []pointerInfo{{Pointer: "x"}, {Pointer: "y"}}
	b := []pointerInfo{{Pointer: "y"}, {Pointer: "z"}}
	out := mergePointers(a, b)
	if len(out) != 3 {
		t.Errorf("got %+v", out)
	}
}

func TestModelSlug(t *testing.T) {
	cases := map[string]string{
		"gpt-image-2":   "gpt-5-3",
		"GPT-Image-2":   "gpt-5-3",
		"auto":          "auto",
		"gpt-5-3-mini":  "gpt-5-3",
		"unknown-model": "auto",
	}
	for in, want := range cases {
		if got := modelSlug(in); got != want {
			t.Errorf("modelSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildPrompt_EditWrapping(t *testing.T) {
	p := buildPrompt("make it red", true, "")
	if !strings.HasPrefix(p, "make it red") {
		t.Errorf("got: %q", p)
	}
	if !strings.Contains(p, improviseHint) {
		t.Errorf("missing improvise hint in: %q", p)
	}
}

func TestBuildPrompt_GenerationPassthrough(t *testing.T) {
	p := buildPrompt("a tiger", false, "")
	if !strings.HasPrefix(p, "a tiger") {
		t.Errorf("got: %q", p)
	}
	if !strings.Contains(p, improviseHint) {
		t.Errorf("missing improvise hint in: %q", p)
	}
}

func TestBuildPrompt_EmptyPromptWithUploads(t *testing.T) {
	p := buildPrompt("", true, "")
	if !strings.HasPrefix(p, "请编辑附带的图片。") {
		t.Errorf("got: %q", p)
	}
	if !strings.Contains(p, improviseHint) {
		t.Errorf("missing improvise hint in: %q", p)
	}
}

func TestBuildPrompt_EmptyPromptNoUploads(t *testing.T) {
	if p := buildPrompt("", false, ""); p != "" {
		t.Errorf("expect empty, got %q", p)
	}
	p := buildPrompt("", false, "1024x1024")
	if strings.Contains(p, improviseHint) {
		t.Errorf("should not append improvise hint when prompt is empty: %q", p)
	}
	if !strings.Contains(p, "1:1 正方形") {
		t.Errorf("expect ratio hint, got %q", p)
	}
}

func TestBuildPrompt_AspectRatioHints(t *testing.T) {
	cases := map[string]string{
		"1024x1024": "1:1 正方形",
		"1792x1024": "16:9 横屏",
		"1024x1792": "9:16 竖屏",
		"1536x1024": "3:2",
		"1:1":       "1:1 正方形",
		"":          "",
		"auto":      "",
	}
	for size, want := range cases {
		p := buildPrompt("a tiger", false, size)
		if !strings.Contains(p, improviseHint) {
			t.Errorf("size=%q missing improvise hint: %q", size, p)
		}
		if want == "" {
			if strings.Contains(p, "构图") || strings.Contains(p, "比例") || strings.Contains(p, "宽高比") {
				t.Errorf("size=%q expect no ratio hint, got %q", size, p)
			}
			continue
		}
		if !strings.Contains(p, want) {
			t.Errorf("size=%q expect contain %q, got %q", size, want, p)
		}
	}
}

func TestLooksLikeTextResponse(t *testing.T) {
	if !looksLikeTextResponse("Sorry, I can't generate that image.") {
		t.Error("expect detect")
	}
	if looksLikeTextResponse("Here is your image: <pointer>") {
		t.Error("expect false on normal text")
	}
}

func TestBuildUploadPointerSet(t *testing.T) {
	set := buildUploadPointerSet([]uploadedFile{{FileID: "a"}, {FileID: "b"}})
	if _, ok := set["file-service://a"]; !ok {
		t.Error("a missing")
	}
	if _, ok := set["file-service://b"]; !ok {
		t.Error("b missing")
	}
}
