package webdriver

import (
	"strings"
	"testing"
)

func TestBuildImagePrompt_AlignsWithChatGPT2API(t *testing.T) {
	out := BuildImagePrompt("一只橘猫宇航员", "1024x1024", "auto", 1)
	if !strings.Contains(out, "一只橘猫宇航员") {
		t.Fatalf("missing base prompt: %q", out)
	}
	if !strings.Contains(out, "请直接生成图片") {
		t.Fatalf("missing force-generate hint: %q", out)
	}
	if !strings.Contains(out, "输出图片尺寸为 1024x1024") {
		t.Fatalf("missing size hint: %q", out)
	}
	if !strings.Contains(out, "正方形 1:1") {
		t.Fatalf("missing square aspect: %q", out)
	}
	if !strings.Contains(out, "输出图片质量为 auto") {
		t.Fatalf("missing quality hint: %q", out)
	}
}

func TestBuildImagePrompt_LandscapeAndCount(t *testing.T) {
	out := BuildImagePrompt("sunset", "1536x1024", "high", 2)
	if !strings.Contains(out, "1536x1024") {
		t.Fatalf("size: %q", out)
	}
	if !strings.Contains(out, "横向 3:2") {
		t.Fatalf("aspect: %q", out)
	}
	if !strings.Contains(out, "请生成 2 张图片") {
		t.Fatalf("count: %q", out)
	}
	if !strings.Contains(out, "输出图片质量为 high") {
		t.Fatalf("quality: %q", out)
	}
}

func TestBuildImagePrompt_EmptySizeSkipsSizeHint(t *testing.T) {
	out := BuildImagePrompt("cat", "", "", 1)
	if strings.Contains(out, "输出图片尺寸") {
		t.Fatalf("unexpected size hint: %q", out)
	}
	if !strings.Contains(out, "请直接生成图片") {
		t.Fatalf("still need force-generate: %q", out)
	}
}
