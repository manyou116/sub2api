package openaiimages

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

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
		uploads = append(uploads, webdriver.Upload{
			Filename:    img.Filename,
			ContentType: img.ContentType,
			Data:        img.Data,
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
			Reason:    rl.Error(),
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
