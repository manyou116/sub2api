package webdriver

import (
	"fmt"
	"strconv"
	"strings"
)

// BuildImagePrompt mirrors chatgpt2api services.protocol.conversation.build_image_prompt:
// append size/quality hints so ChatGPT Web actually generates an image with the
// requested dimensions, instead of only describing it.
//
// Extra (compatible) enhancements:
//   - force-generate instruction (reduce pure-text replies)
//   - aspect-ratio phrasing when WxH can be parsed
//   - multi-image count when n > 1
func BuildImagePrompt(prompt, size, quality string, n int) string {
	base := strings.TrimSpace(prompt)
	if base == "" {
		base = "Generate an image."
	}

	var hints []string
	// Always push the model toward tool/image generation (web chat path has no forced tool_choice).
	hints = append(hints, "请直接生成图片，不要只用文字描述。")

	size = strings.TrimSpace(size)
	if size != "" && !strings.EqualFold(size, "auto") {
		if ratio := aspectRatioHint(size); ratio != "" {
			hints = append(hints, fmt.Sprintf("输出图片尺寸为 %s（%s）。", size, ratio))
		} else {
			hints = append(hints, fmt.Sprintf("输出图片尺寸为 %s。", size))
		}
	}

	quality = strings.TrimSpace(quality)
	if quality != "" {
		// Match chatgpt2api: include quality even when "auto".
		hints = append(hints, fmt.Sprintf("输出图片质量为 %s。", quality))
	}

	if n > 1 {
		hints = append(hints, fmt.Sprintf("请生成 %d 张图片。", n))
	}

	if len(hints) == 0 {
		return base
	}
	return base + "\n\n" + strings.Join(hints, "")
}

// aspectRatioHint returns a short Chinese aspect description for common OpenAI sizes.
func aspectRatioHint(size string) string {
	w, h, ok := parseWxH(size)
	if !ok || w <= 0 || h <= 0 {
		return ""
	}
	if w == h {
		return "正方形 1:1"
	}
	g := gcd(w, h)
	rw, rh := w/g, h/g
	// Prefer friendly labels for common image API sizes.
	switch {
	case w > h && rw == 3 && rh == 2:
		return "横向 3:2"
	case h > w && rw == 2 && rh == 3:
		return "纵向 2:3"
	case w > h && rw == 16 && rh == 9:
		return "横向 16:9"
	case h > w && rw == 9 && rh == 16:
		return "纵向 9:16"
	case w > h && rw == 7 && rh == 4: // 1792x1024 ≈ 7:4
		return "横向宽屏"
	case h > w && rw == 4 && rh == 7: // 1024x1792
		return "纵向长图"
	case w > h:
		return fmt.Sprintf("横向 %d:%d", rw, rh)
	default:
		return fmt.Sprintf("纵向 %d:%d", rw, rh)
	}
}

func parseWxH(size string) (int, int, bool) {
	size = strings.ToLower(strings.TrimSpace(size))
	size = strings.ReplaceAll(size, " ", "")
	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return w, h, true
}

func gcd(a, b int) int {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}
