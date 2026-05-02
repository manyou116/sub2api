package webdriver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/imroc/req/v3"
)

// uploadFiles 执行 ChatGPT 三步上传协议：
//  1. POST /backend-api/files          → 获取 file_id 和 Azure upload_url
//  2. PUT upload_url (Azure Blob Storage)
//  3. POST /backend-api/files/{id}/uploaded → 确认完成
func uploadFiles(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	baseFilesURL string,
	uploads []Upload,
) ([]uploadedFile, error) {
	if len(uploads) == 0 {
		return nil, nil
	}
	results := make([]uploadedFile, 0, len(uploads))
	for _, up := range uploads {
		if len(up.Data) == 0 {
			continue
		}
		fileName := coalesce(up.Filename, "image.png")
		ct := coalesce(up.ContentType, "image/png")

		var created struct {
			FileID    string `json:"file_id"`
			UploadURL string `json:"upload_url"`
		}
		filesPath := targetPathOf(baseFilesURL)
		// 与 chatgpt2api `_upload_image` 对齐：create-upload-slot body 不包含 content_type。
		resp, err := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(withTargetPath(headers, filesPath))).
			SetBodyJsonMarshal(map[string]any{
				"file_name": fileName,
				"file_size": len(up.Data),
				"use_case":  "multimodal",
				"width":     up.Width,
				"height":    up.Height,
			}).
			SetSuccessResult(&created).
			Post(baseFilesURL)
		if err != nil {
			return nil, &TransportError{Wrapped: fmt.Errorf("create upload slot: %w", err)}
		}
		if !resp.IsSuccessState() || strings.TrimSpace(created.FileID) == "" {
			return nil, classifyHTTPError(resp, "create upload slot failed")
		}

		if u := strings.TrimSpace(created.UploadURL); u != "" {
			// 与 chatgpt2api 对齐：在 create slot 与 PUT 之间 sleep 500ms，
			// 否则部分账号上 Azure SAS token 还未生效，PUT 直接 403。
			time.Sleep(500 * time.Millisecond)
			ua := headers.Get("User-Agent")
			// 与 chatgpt2api 对齐：Azure PUT 使用一组显式的浏览器导航头，
			// 不要把 backend-api 用的 Authorization / OAI-* 头带上去（Azure 会拒绝）。
			resp2, err2 := client.R().
				SetContext(ctx).
				SetHeader("Content-Type", ct).
				SetHeader("x-ms-blob-type", "BlockBlob").
				SetHeader("x-ms-version", "2020-04-08").
				SetHeader("Origin", chatgptOrigin).
				SetHeader("Referer", chatgptReferer).
				SetHeader("User-Agent", ua).
				SetHeader("Accept", "application/json, text/plain, */*").
				SetHeader("Accept-Language", "en-US,en;q=0.8").
				SetBodyBytes(up.Data).
				Put(u)
			if err2 != nil {
				return nil, &TransportError{Wrapped: fmt.Errorf("azure upload: %w", err2)}
			}
			if !resp2.IsSuccessState() {
				return nil, classifyHTTPError(resp2, "azure blob upload failed")
			}
		}

		uploadedURL := fmt.Sprintf("%s/%s/uploaded", baseFilesURL, created.FileID)
		// 与 chatgpt2api 对齐：confirm 请求的 body 是字符串 "{}"。
		resp3, err3 := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(withTargetPath(headers, targetPathOf(uploadedURL)))).
			SetBodyString("{}").
			Post(uploadedURL)
		if err3 != nil {
			return nil, &TransportError{Wrapped: fmt.Errorf("confirm upload: %w", err3)}
		}
		if !resp3.IsSuccessState() {
			return nil, classifyHTTPError(resp3, "confirm upload failed")
		}

		results = append(results, uploadedFile{
			FileID: created.FileID, FileName: fileName, FileSize: len(up.Data),
			ContentType: ct, Width: up.Width, Height: up.Height,
		})
	}
	return results, nil
}
