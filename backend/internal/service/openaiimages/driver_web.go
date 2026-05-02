package openaiimages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
)

// WebDriverAdapter 把 webdriver.Driver 适配为 openaiimages.Driver 接口。
//
// 适配层职责：
//   - 把 ImagesRequest + AccountView 投影成 webdriver.Request
//   - 把 webdriver.Result 还原为 ImageResult（含 base64 编码 / mime 推断）
//   - 把 webdriver typed error 翻译成 openaiimages typed error
type WebDriverAdapter struct {
	inner *webdriver.Driver
}

// NewWebDriverAdapter 用默认 endpoints 构造适配器。
func NewWebDriverAdapter() *WebDriverAdapter {
	return &WebDriverAdapter{inner: webdriver.New(webdriver.Endpoints{})}
}

// NewWebDriverAdapterWith 允许注入自定义 endpoints（测试或 staging 用）。
func NewWebDriverAdapterWith(endpoints webdriver.Endpoints) *WebDriverAdapter {
	return &WebDriverAdapter{inner: webdriver.New(endpoints)}
}

func (d *WebDriverAdapter) Name() string { return DriverWeb }

func (d *WebDriverAdapter) Forward(ctx context.Context, account AccountView, req *ImagesRequest) (*ImageResult, error) {
	if account == nil || account.AccessToken() == "" {
		return nil, &AuthError{HTTPStatus: 401, Reason: "missing access token for web driver"}
	}
	if req == nil {
		return nil, errors.New("openaiimages: nil request")
	}

	uploads := make([]webdriver.Upload, 0, len(req.Images))
	for _, img := range req.Images {
		w, h := decodeImageDimensions(img.Data)
		uploads = append(uploads, webdriver.Upload{
			Filename:    normalizeImageFilename(img.Filename, img.ContentType),
			ContentType: img.ContentType,
			Data:        img.Data,
			Width:       w,
			Height:      h,
		})
	}

	// ChatGPT Web 单次 conversation 只产出一张图，n>1 在适配层做并行 fan-out。
	n := req.N
	if n <= 0 {
		n = 1
	}

	type result struct {
		items []ImageItem
		err   error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			wreq := &webdriver.Request{
				Account: webdriver.AccountInfo{
					AccountID:        account.ID(),
					AccessToken:      account.AccessToken(),
					ChatGPTAccountID: account.ChatGPTAccountID(),
					UserAgent:        account.UserAgent(),
					DeviceID:         account.DeviceID(),
					SessionID:        account.SessionID(),
					ProxyURL:         account.ProxyURL(),
				},
				Model:          req.Model,
				Prompt:         req.Prompt,
				Size:           req.Size,
				N:              1,
				Uploads:        uploads,
				AllowEarlyExit: true,
				ResponseFormat: string(req.ResponseFormat),
			}
			wres, err := d.inner.Forward(ctx, wreq)
			if err != nil {
				results[idx] = result{err: translateWebError(err)}
				return
			}
			items := make([]ImageItem, 0, len(wres.Images))
			for _, img := range wres.Images {
				mt := img.ContentType
				if mt == "" {
					mt = "image/png"
				}
				items = append(items, ImageItem{
					B64JSON:  encodeBase64(img.Bytes),
					MimeType: mt,
					Bytes:    img.Bytes,
				})
			}
			results[idx] = result{items: items}
		}(i)
	}
	wg.Wait()

	allItems := make([]ImageItem, 0, n)
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		allItems = append(allItems, r.items...)
	}
	// 全部失败才向上抛错；部分成功则降级返回（可观测性靠日志/usage）
	if len(allItems) == 0 && firstErr != nil {
		return nil, firstErr
	}

	return &ImageResult{
		Items:   allItems,
		Model:   req.Model,
		Created: time.Now().Unix(),
	}, nil
}

// translateWebError 把 webdriver 的 typed error 映射到 openaiimages 的 typed error。
func translateWebError(err error) error {
	if err == nil {
		return nil
	}

	var rl *webdriver.RateLimitError
	if errors.As(err, &rl) {
		return &RateLimitError{
			HTTPStatus: 429,
			Reason:     rl.Error(),
			ResetAfter: rl.ResetAfter,
		}
	}

	var au *webdriver.AuthError
	if errors.As(err, &au) {
		return &AuthError{HTTPStatus: 401, Reason: au.Error()}
	}

	var cp *webdriver.ContentPolicyError
	if errors.As(err, &cp) {
		return &ContentPolicyError{UpstreamMessage: cp.UpstreamMessage}
	}

	var ni *webdriver.ModelNoImageError
	if errors.As(err, &ni) {
		return &ModelNoImageError{UpstreamMessage: ni.UpstreamMessage}
	}

	var pe *webdriver.ProtocolError
	if errors.As(err, &pe) {
		return &UpstreamError{HTTPStatus: 502, Reason: pe.Error()}
	}

	var te *webdriver.TransportError
	if errors.As(err, &te) {
		return &TransportError{Reason: te.Error()}
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "context"):
		return &TransportError{Reason: msg}
	}
	return &UpstreamError{HTTPStatus: 502, Reason: fmt.Sprintf("webdriver: %s", msg)}
}

// decodeImageDimensions 从原始图片字节推断宽高，失败时返回 (0, 0)。
// 只读取头部字节，性能影响可忽略。
func decodeImageDimensions(data []byte) (width, height int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// normalizeImageFilename 修正来自客户端的文件名：
//   - 浏览器 Blob 对象默认文件名 "blob"（无扩展名）→ 替换为 "image.<ext>"
//   - 空文件名 → 替换为 "image.<ext>"
//   - 无图片扩展名 → 追加正确扩展名
//
// ChatGPT 模型依赖 attachment 的 name 字段（含扩展名）判断是否调用 image_generation tool，
// 文件名为 "blob" 时模型不会将其识别为图片，导致不调用 tool、edit 失败。
func normalizeImageFilename(filename, contentType string) string {
	ext := extFromMIME(contentType)
	name := strings.TrimSpace(filename)
	if name == "" || name == "blob" {
		return "image" + ext
	}
	// 如果已有图片扩展名，直接返回。
	lower := strings.ToLower(name)
	for _, e := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tiff"} {
		if strings.HasSuffix(lower, e) {
			return name
		}
	}
	// 没有扩展名，追加。
	return name + ext
}

func extFromMIME(ct string) string {
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/tiff":
		return ".tiff"
	default:
		return ".png"
	}
}
