package webdriver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/imroc/req/v3"
)

// retriable 判定 fetchDownloadURL/downloadBytes 是否值得重试。
// TransportError（含 5xx / cf-challenge / 网络抖动）和 sediment 的临时 404 都重试。
func retriable(err error) bool {
	if err == nil {
		return false
	}
	var te *TransportError
	return errors.As(err, &te)
}

// fetchDownloadURL 把 file-service://{id} 或 sediment://{id} 解析成可直下的 URL。
// pointer 类型决定调用哪个 ChatGPT 端点。
func fetchDownloadURL(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	baseFilesURL, baseConvURL string,
	conversationID string,
	pointer string,
) (string, error) {
	var url string
	var allowRetry bool
	switch {
	case strings.HasPrefix(pointer, "file-service://"):
		url = fmt.Sprintf("%s/%s/download", baseFilesURL, strings.TrimPrefix(pointer, "file-service://"))
	case strings.HasPrefix(pointer, "sediment://"):
		attachmentID := strings.TrimPrefix(pointer, "sediment://")
		url = fmt.Sprintf("%s/%s/attachment/%s/download", baseConvURL, conversationID, attachmentID)
		allowRetry = true
	default:
		return "", fmt.Errorf("unsupported pointer: %s", pointer)
	}

	var lastErr error
	const maxAttempts = 6
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var result struct {
			DownloadURL string `json:"download_url"`
		}
		resp, err := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(headers)).
			SetSuccessResult(&result).
			Get(url)
		if err != nil {
			lastErr = &TransportError{Wrapped: err}
		} else if resp.IsSuccessState() && strings.TrimSpace(result.DownloadURL) != "" {
			return strings.TrimSpace(result.DownloadURL), nil
		} else {
			classified := classifyHTTPError(resp, "fetch download url failed")
			// sediment 临时 404（资源还没 ready）总是重试。
			isSedimentNotFound := allowRetry && resp != nil && resp.StatusCode == http.StatusNotFound
			if !isSedimentNotFound && !retriable(classified) {
				return "", classified
			}
			lastErr = classified
		}
		if attempt == maxAttempts-1 {
			break
		}
		// 指数退避：500ms / 1s / 2s / 4s / 6s
		backoff := time.Duration(500*(1<<uint(attempt))) * time.Millisecond
		if backoff > 6*time.Second {
			backoff = 6 * time.Second
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C:
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("fetch download url failed")
	}
	return "", lastErr
}

// downloadBytes 下载 download_url 指向的图片字节。对 transport / 5xx 自动重试 3 次。
func downloadBytes(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	downloadURL string,
) ([]byte, string, error) {
	var lastErr error
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		data, ct, err := downloadBytesOnce(ctx, client, headers, downloadURL)
		if err == nil {
			return data, ct, nil
		}
		lastErr = err
		if !retriable(err) {
			return nil, "", err
		}
		if attempt == maxAttempts-1 {
			break
		}
		backoff := time.Duration(500*(1<<uint(attempt))) * time.Millisecond
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, "", ctx.Err()
		case <-t.C:
		}
	}
	return nil, "", lastErr
}

func downloadBytesOnce(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	downloadURL string,
) ([]byte, string, error) {
	r := client.R().SetContext(ctx).DisableAutoReadResponse()
	if strings.HasPrefix(downloadURL, "https://chatgpt.com") {
		r = r.SetHeaders(headerToMap(headers))
	}
	resp, err := r.Get(downloadURL)
	if err != nil {
		return nil, "", &TransportError{Wrapped: fmt.Errorf("download image: %w", err)}
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if !resp.IsSuccessState() {
		return nil, "", classifyHTTPError(resp, "download image failed")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageDownloadBytes))
	if err != nil {
		return nil, "", &TransportError{Wrapped: fmt.Errorf("read image: %w", err)}
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(data)
	}
	return data, ct, nil
}
