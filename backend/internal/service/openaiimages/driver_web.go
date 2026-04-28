package openaiimages

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
		N:              req.N,
		Uploads:        uploads,
		AllowEarlyExit: len(uploads) == 0,
		ResponseFormat: string(req.ResponseFormat),
	}

	wres, err := d.inner.Forward(ctx, wreq)
	if err != nil {
		return nil, translateWebError(err)
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

	return &ImageResult{
		Items:   items,
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
