package webdriver

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/imroc/req/v3"
)

// uploadFiles 执行 ChatGPT 三步上传协议：
//   1. POST /backend-api/files          → 获取 file_id 和 Azure upload_url
//   2. PUT upload_url (Azure Blob Storage)
//   3. POST /backend-api/files/{id}/uploaded → 确认完成
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
		resp, err := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(headers)).
			SetBodyJsonMarshal(map[string]any{
				"file_name":    fileName,
				"file_size":    len(up.Data),
				"use_case":     "multimodal",
				"content_type": ct,
				"width":        up.Width,
				"height":       up.Height,
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
			resp2, err2 := client.R().
				SetContext(ctx).
				SetHeader("x-ms-blob-type", "BlockBlob").
				SetHeader("x-ms-version", "2020-04-08").
				SetHeader("Content-Type", ct).
				SetBodyBytes(up.Data).
				Put(u)
			if err2 != nil {
				return nil, &TransportError{Wrapped: fmt.Errorf("azure upload: %w", err2)}
			}
			if !resp2.IsSuccessState() {
				return nil, classifyHTTPError(resp2, "azure blob upload failed")
			}
		}

		resp3, err3 := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(headers)).
			SetBodyJsonMarshal(map[string]any{}).
			Post(fmt.Sprintf("%s/%s/uploaded", baseFilesURL, created.FileID))
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
