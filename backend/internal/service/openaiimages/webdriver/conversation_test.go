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
	p := buildPrompt("make it red", true)
	if !strings.Contains(p, "Generate the image directly") || !strings.Contains(p, "make it red") {
		t.Errorf("got: %s", p)
	}
}

func TestBuildPrompt_GenerationPassthrough(t *testing.T) {
	p := buildPrompt("a tiger", false)
	if !strings.Contains(p, "Generate the image directly") || !strings.Contains(p, "a tiger") {
		t.Errorf("got: %s", p)
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
