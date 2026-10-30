package service

import (
	"context"
	"errors"
	"sync"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const MaxWebImageShadowBatch = 200

type WebImageShadowBatchInput struct {
	ParentIDs   []int64  `json:"parent_ids"`
	Modes       []string `json:"modes,omitempty"`
	Concurrency *int     `json:"concurrency,omitempty"`
}

type WebImageShadowBatchItem struct {
	ParentID int64  `json:"parent_id"`
	Mode     string `json:"mode"`
	ShadowID int64  `json:"shadow_id,omitempty"`
	Status   string `json:"status"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

type WebImageShadowBatchResult struct {
	Created  int                       `json:"created"`
	Existing int                       `json:"existing"`
	Skipped  int                       `json:"skipped"`
	Failed   int                       `json:"failed"`
	Results  []WebImageShadowBatchItem `json:"results"`
}

type webImageShadowCreator interface {
	CreateWebImageShadow(context.Context, int64, WebImageShadowOptions) (*Account, error)
}

// CreateWebImageShadows reuses single-account validation and uniqueness. Existing
// children are left untouched, making retries safe after a partially completed batch.
func CreateWebImageShadows(ctx context.Context, creator webImageShadowCreator, input WebImageShadowBatchInput) (*WebImageShadowBatchResult, error) {
	if len(input.ParentIDs) == 0 || len(input.ParentIDs) > MaxWebImageShadowBatch {
		return nil, infraerrors.BadRequest("WEB_IMAGE_SHADOW_BATCH_SIZE", "parent_ids must contain between 1 and 200 IDs")
	}
	if input.Concurrency != nil && *input.Concurrency <= 0 {
		return nil, infraerrors.BadRequest("WEB_IMAGE_SHADOW_CONCURRENCY", "Web image shadow concurrency must be positive")
	}
	modes := make([]string, 0, 2)
	if len(input.Modes) == 0 {
		input.Modes = []string{ImageShadowModeWeb}
	}
	for _, requested := range input.Modes {
		mode, err := normalizeImageShadowMode(requested)
		if err != nil || requested == "" {
			return nil, infraerrors.BadRequest("IMAGE_SHADOW_MODE_INVALID", "modes must contain web or work")
		}
		if len(modes) == 0 || (modes[0] != mode && len(modes) == 1) {
			modes = append(modes, mode)
		}
	}
	result := &WebImageShadowBatchResult{Results: make([]WebImageShadowBatchItem, 0, len(input.ParentIDs)*len(modes))}
	seen := make(map[int64]bool, len(input.ParentIDs))
	for _, id := range input.ParentIDs {
		if id <= 0 {
			return nil, infraerrors.BadRequest("WEB_IMAGE_SHADOW_PARENT_ID", "parent IDs must be positive")
		}
		if !seen[id] {
			seen[id] = true
			for _, mode := range modes {
				result.Results = append(result.Results, WebImageShadowBatchItem{
					ParentID: id, Mode: mode, Status: "failed", Code: "WEB_IMAGE_SHADOW_BATCH_CANCELED", Message: "batch canceled before this account was created",
				})
			}
		}
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(4, len(result.Results)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				item := &result.Results[i]
				shadow, err := creator.CreateWebImageShadow(ctx, item.ParentID, WebImageShadowOptions{Mode: item.Mode, Concurrency: input.Concurrency})
				if err == nil && shadow != nil {
					*item = WebImageShadowBatchItem{ParentID: item.ParentID, Mode: item.Mode, ShadowID: shadow.ID, Status: "created"}
					continue
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					continue
				}
				item.Code, item.Message = infraerrors.Reason(err), infraerrors.Message(err)
				switch item.Code {
				case "WEB_IMAGE_SHADOW_ALREADY_EXISTS":
					item.Status = "existing"
				case "WEB_IMAGE_SHADOW_INVALID_PARENT", "ACCOUNT_NOT_FOUND":
					item.Status = "skipped"
				default:
					if item.Code == "" {
						item.Code, item.Message = "WEB_IMAGE_SHADOW_CREATE_FAILED", "could not create Web image shadow"
					}
				}
			}
		}()
	}
dispatch:
	for i := range result.Results {
		select {
		case <-ctx.Done():
			break dispatch
		case jobs <- i:
		}
	}
	close(jobs)
	workers.Wait()
	for _, item := range result.Results {
		switch item.Status {
		case "created":
			result.Created++
		case "existing":
			result.Existing++
		case "skipped":
			result.Skipped++
		case "failed":
			result.Failed++
		}
	}
	return result, nil
}
