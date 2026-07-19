package service

import (
	"strings"
	"testing"
)

func TestAppendOpenAIWebImagesDownloadAttachmentPrompt(t *testing.T) {
	out := appendOpenAIWebImagesDownloadAttachmentPrompt("生成海报", "2160x3840")
	for _, want := range []string{
		"生成海报",
		"请使用图像生成流程创建原图文件。",
		"画布严格为 2160x3840 像素。",
		"最终将 PNG 作为可下载文件/附件保存并提供下载链接。",
		"不要只发送聊天内预览图或压缩图。",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestAppendOpenAIWebImagesDownloadAttachmentPromptSkipsInvalidSize(t *testing.T) {
	out := appendOpenAIWebImagesDownloadAttachmentPrompt("生成海报", "4k")
	if out != "生成海报" {
		t.Fatalf("unexpected prompt: %q", out)
	}
}

func TestInferOpenAIWebImageTestSize(t *testing.T) {
	got := inferOpenAIWebImageTestSize("画布严格为 2160×3840 像素，输出图片尺寸为 2160x3840。")
	if got != "2160x3840" {
		t.Fatalf("got %q", got)
	}
}
