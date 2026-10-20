package webdriver

import (
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/url"
	"strings"

	"github.com/imroc/req/v3"
	_ "golang.org/x/image/webp"
)

const maxDownloadedImageBytes = 20 << 20

func chatGPTDownloadURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com")
}

func readDownloadBody(resp *req.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	limit := int64(maxDownloadedImageBytes)
	if resp.StatusCode >= 400 {
		limit = 1 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, transportError("download", err)
	}
	if resp.StatusCode >= 400 {
		return nil, classifyHTTP("download", resp.StatusCode, string(data), resp.Header)
	}
	if int64(len(data)) > limit {
		return nil, NewError(ErrorKindUpstream, "download", "image exceeds download size limit", 0, false)
	}
	return data, nil
}

func validateDownloadedImage(data []byte) error {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 32768 || config.Height > 32768 || int64(config.Width)*int64(config.Height) > 100_000_000 {
		return NewError(ErrorKindUpstream, "download", "invalid image format or dimensions", 0, false)
	}
	return nil
}
