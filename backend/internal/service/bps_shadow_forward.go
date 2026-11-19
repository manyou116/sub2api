package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type bpsResponsesKey struct{}

func WithBPSResponses(ctx context.Context, model string) context.Context {
	if strings.HasPrefix(model, bpsModelPrefix) {
		return context.WithValue(ctx, bpsResponsesKey{}, true)
	}
	return ctx
}

func bpsRouteMismatch(ctx context.Context, a *Account, model string, compact bool) bool {
	isBPS := strings.HasPrefix(model, bpsModelPrefix)
	if !isBPS && !a.IsBPSShadow() {
		return false
	}
	return !isBPS || !a.IsBPSShadow() || compact || ctx.Value(bpsResponsesKey{}) != true
}

func bpsTargetModel(child *Account) string {
	for alias, model := range child.GetModelMapping() {
		if alias == bpsModelPrefix+model && bpsModelPattern.MatchString(model) {
			return model
		}
	}
	return ""
}

func bpsProbe(ctx context.Context, p bpsbridge.Process, temp string, bound bpsbridge.BoundGrant, child *Account) error {
	model := bpsTargetModel(child)
	if model == "" {
		return bpsbridge.ErrUnavailable
	}
	auth := filepath.Join(temp, "probe.json")
	if err := bpsbridge.Save(auth, bound); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	worker, err := p.Start(ctx, auth, filepath.Join(temp, "task"), 1)
	if err != nil {
		return err
	}
	defer worker.Stop()
	body, _ := json.Marshal(map[string]any{"model": model, "input": "Reply only OK.", "stream": true, "max_output_tokens": 64})
	resp, err := worker.Responses(ctx, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if !bpsIsStream(resp) {
		return bpsbridge.ErrUnavailable
	}
	terminal, err := bpsReadStream(resp.Body, 32<<10, func(data string, _ []byte) error {
		event := gjson.Parse(data)
		if event.Get("type").String() == "response.completed" && strings.TrimSpace(event.Get(`response.output.#(type=="message").content.#(type=="output_text").text`).String()) == "" {
			return bpsbridge.ErrUnavailable
		}
		return nil
	})
	if err != nil || terminal.Get("type").String() != "response.completed" {
		return bpsbridge.ErrUnavailable
	}
	return nil
}

type bpsRuntimeKey struct {
	shadowID    int64
	proxy       string
	concurrency int
}
type bpsRuntimeWorker struct {
	mu     sync.Mutex
	key    bpsRuntimeKey
	worker *bpsbridge.Worker
	users  int
	idle   *time.Timer
}

var bpsWorkers sync.Map

func bpsAcquireWorker(ctx context.Context, p bpsbridge.Process, child *Account) (*bpsbridge.Worker, func(), error) {
	key := bpsRuntimeKey{child.ID, p.Proxy, child.Concurrency}
	entry, _ := bpsWorkers.LoadOrStore(child.ID, &bpsRuntimeWorker{})
	slot, ok := entry.(*bpsRuntimeWorker)
	if !ok {
		return nil, nil, bpsbridge.ErrUnavailable
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.worker != nil {
		select {
		case <-slot.worker.Done:
			slot.worker = nil
		default:
		}
	}
	if slot.worker != nil && slot.key != key {
		if slot.users != 0 {
			return nil, nil, bpsbridge.ErrUnavailable
		}
		slot.worker.Stop()
		slot.worker = nil
	}
	if slot.worker == nil {
		worker, err := p.Start(ctx, bpsGrantPath(p), filepath.Join(p.Dir, "task"), child.Concurrency)
		if err != nil {
			return nil, nil, err
		}
		slot.worker, slot.key = worker, key
	}
	slot.users++
	if slot.idle != nil {
		slot.idle.Stop()
	}
	return slot.worker, func() {
		slot.mu.Lock()
		defer slot.mu.Unlock()
		slot.users--
		if slot.users == 0 && slot.worker != nil {
			worker := slot.worker
			slot.idle = time.AfterFunc(time.Minute, func() {
				slot.mu.Lock()
				defer slot.mu.Unlock()
				if slot.users == 0 && slot.worker == worker {
					slot.worker.Stop()
					slot.worker = nil
				}
			})
		}
	}, nil
}

func bpsIsStream(resp *http.Response) bool {
	return resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
}

func bpsReadStream(r io.Reader, maxLine int, write func(string, []byte) error) (gjson.Result, error) {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 16<<10), maxLine)
	for scan.Scan() {
		line := scan.Text()
		data, ok := extractOpenAISSEDataLine(line)
		if !ok {
			continue
		} // The CLI emits one compact JSON data line per event.
		if !gjson.Valid(data) {
			return gjson.Result{}, bpsbridge.ErrUnavailable
		}
		event := gjson.Parse(data)
		switch event.Get("type").String() {
		case "response.completed", "response.incomplete", "response.failed", "error":
			return event, write(data, []byte(line))
		default:
			if err := write(data, []byte(line)); err != nil {
				return gjson.Result{}, err
			}
		}
	}
	if err := scan.Err(); err != nil {
		return gjson.Result{}, err
	}
	return gjson.Result{}, io.ErrUnexpectedEOF
}

func bpsSharedQuotaReset(errPayload gjson.Result, now time.Time) (time.Time, bool) {
	if errPayload.Get("type").String() != "usage_limit_reached" &&
		errPayload.Get("code").String() != "usage_limit_reached" {
		return time.Time{}, false
	}
	unix := errPayload.Get("resets_at").Int()
	if unix <= 0 {
		return time.Time{}, false
	}
	reset := time.Unix(unix, 0)
	return reset, reset.After(now) && reset.Before(now.Add(8*24*time.Hour))
}

func (s *OpenAIGatewayService) forwardBPS(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	fail := func(status int, code string) (*OpenAIForwardResult, error) {
		c.JSON(status, gin.H{"error": gin.H{"type": code, "message": code}})
		return nil, errors.New(code) // Never a scheduler failover or retry.
	}
	start := time.Now()
	if c.Request.Context().Value(bpsResponsesKey{}) != true {
		return fail(http.StatusBadRequest, "bps_responses_only")
	}
	model := gjson.GetBytes(body, "model").String()
	mapped := bpsTargetModel(account)
	if mapped == "" || model != bpsModelPrefix+mapped || !gjson.GetBytes(body, "stream").Bool() ||
		gjson.GetBytes(body, "previous_response_id").String() != "" || OpenAIImageGenerationIntentFromContext(ctx) {
		return fail(http.StatusBadRequest, "bps_requires_streaming_and_full_history")
	}
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		switch tool.Get("type").String() {
		case "function", "custom", "namespace":
		default:
			return fail(http.StatusBadRequest, "bps_hosted_tool_unsupported")
		}
	}
	bound, p, err := bpsReadBound(account.ID)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "bps_authorization_required")
	}
	parent, err := s.accountRepo.GetByID(ctx, *account.ParentAccountID)
	if err != nil || !bpsMatchesParent(bound, parent) {
		return fail(http.StatusServiceUnavailable, "bps_authorization_required")
	}
	if !bpsParentQuotaAllows(ctx, parent, mapped) {
		return fail(http.StatusTooManyRequests, "bps_shared_quota_unavailable")
	}
	if parent.ProxyID != nil {
		if parent.Proxy == nil {
			return fail(http.StatusServiceUnavailable, "bps_parent_proxy_unavailable")
		}
		p.Proxy = parent.Proxy.URL()
	}
	body, err = sjson.SetBytes(body, "model", mapped)
	if err != nil {
		return fail(http.StatusBadRequest, "bps_invalid_request")
	}
	worker, release, err := bpsAcquireWorker(ctx, p, account)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "bps_bridge_unavailable")
	}
	defer release()
	resp, err := worker.Responses(ctx, body)
	if err != nil {
		return fail(http.StatusBadGateway, "bps_result_unconfirmed")
	}
	defer func() { _ = resp.Body.Close() }()
	if !bpsIsStream(resp) {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		if resp.StatusCode == http.StatusTooManyRequests {
			s.noteBPSQuota(ctx, parent.ID, gjson.GetBytes(raw, "error"))
			return fail(http.StatusTooManyRequests, "bps_rate_limited")
		}
		return fail(http.StatusBadGateway, "bps_upstream_rejected")
	}
	SetActualOpenAIUpstreamEndpoint(c, "/bps/responses")
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	stats := &OpenAIForwardResult{Model: model, UpstreamModel: mapped, BillingModel: mapped, Stream: true, UpstreamEndpoint: "/bps/responses"}
	terminal, err := bpsReadStream(resp.Body, 16<<20, func(data string, _ []byte) error {
		event := gjson.Parse(data)
		eventType := event.Get("type").String()
		if eventType == "" || strings.ContainsAny(eventType, "\r\n") {
			return bpsbridge.ErrUnavailable
		}
		if stats.FirstTokenMs == nil && strings.HasSuffix(eventType, ".delta") {
			ms := int(time.Since(start).Milliseconds())
			stats.FirstTokenMs = &ms
		}
		if _, err := c.Writer.WriteString("event: " + eventType + "\ndata: " + data + "\n\n"); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			_, _ = c.Writer.WriteString("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"bps_result_unconfirmed\"}}}\n\n")
			c.Writer.Flush()
		}
		return nil, errors.New("bps_result_unconfirmed")
	}
	switch event := terminal.Get("type").String(); event {
	case "response.completed":
		stats.UpstreamResponseModel = terminal.Get("response.model").String()
		stats.ResponseID = terminal.Get("response.id").String()
		stats.RequestID = stats.ResponseID
		stats.Duration = time.Since(start)
		u := terminal.Get("response.usage")
		stats.Usage.InputTokens = int(u.Get("input_tokens").Int())
		stats.Usage.OutputTokens = int(u.Get("output_tokens").Int())
		stats.Usage.CacheReadInputTokens = int(u.Get("input_tokens_details.cached_tokens").Int())
		return stats, nil
	case "response.failed", "error":
		s.noteBPSQuota(ctx, parent.ID, terminal.Get("response.error"))
		s.noteBPSQuota(ctx, parent.ID, terminal.Get("error"))
		return nil, errors.New("bps_generation_failed")
	default:
		return nil, errors.New("bps_generation_incomplete")
	}
}

func bpsParentQuotaAllows(ctx context.Context, parent *Account, model string) bool {
	if parent.IsRateLimited() || parent.isModelRateLimitedWithContext(ctx, model) {
		return false
	}
	paused, _ := shouldAutoPauseOpenAIAccountByQuota(ctx, parent)
	return !paused
}

func (s *OpenAIGatewayService) noteBPSQuota(ctx context.Context, parentID int64, payload gjson.Result) {
	reset, ok := bpsSharedQuotaReset(payload, time.Now())
	if !ok {
		return
	}
	if repo, ok := s.accountRepo.(interface {
		SetRateLimitedIfLater(context.Context, int64, time.Time) error
	}); ok {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = repo.SetRateLimitedIfLater(work, parentID, reset)
	}
}
