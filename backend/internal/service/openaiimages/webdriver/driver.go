package webdriver

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/imroc/req/v3"
	"github.com/tidwall/gjson"
)

const (
	baseURL            = "https://chatgpt.com"
	defaultUA          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	defaultPollTimeout = 180 * time.Second
	// Baseline poll interval floor for post-SSE fallback only (see poll_schedule.go).
	// Keep >= 4s to avoid conversation GET 429 storms.
	defaultPollEvery = 4 * time.Second
	// SSE-only while stream is alive. Poll ONLY after SSE ends/idle/disconnect.
	defaultSSEMaxWait = 120 * time.Second
	// Quiet stream with cid: leave SSE sooner so post-SSE poll can find assets / quota text.
	defaultSSEIdleWait = 2500 * time.Millisecond
	// No conversation_id yet.
	defaultSSEIdleWaitNoCID = 2500 * time.Millisecond
	// After [DONE] without assets, briefly drain trailing SSE lines (late pointers) before poll.
	defaultSSEDrainAfterDone = 800 * time.Millisecond
)

type Driver struct {
	ClientFactory    func(proxyURL string) (*req.Client, error)
	PollTimeout      time.Duration
	PollInterval     time.Duration
	KeepConversation bool // when true, skip PATCH is_visible=false cleanup
}

func NewDriver(factory func(proxyURL string) (*req.Client, error)) *Driver {
	return &Driver{ClientFactory: factory, PollTimeout: defaultPollTimeout, PollInterval: defaultPollEvery}
}

func (d *Driver) ProbeQuota(ctx context.Context, auth Auth) (*Quota, error) {
	client, headers, err := d.session(auth)
	if err != nil {
		return nil, err
	}
	stage := "probe"
	resp, err := client.R().SetContext(ctx).SetHeaders(headers).
		SetHeader("Content-Type", "application/json").
		SetHeader("X-OpenAI-Target-Path", "/backend-api/conversation/init").
		SetHeader("X-OpenAI-Target-Route", "/backend-api/conversation/init").
		SetBody(map[string]any{"gizmo_id": nil, "requested_default_model": nil, "conversation_id": nil, "timezone_offset_min": -480}).
		Post(baseURL + "/backend-api/conversation/init")
	if err != nil {
		return nil, NewError(ErrorKindTransport, stage, err.Error(), 0, true)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, NewError(ErrorKindAuth, stage, "token invalidated", resp.StatusCode, false)
	}
	if resp.StatusCode >= 400 {
		return nil, classifyHTTP(stage, resp.StatusCode, resp.String())
	}
	remaining := 0
	var resetAt *time.Time
	found := false
	for _, item := range gjson.GetBytes(resp.Bytes(), "limits_progress").Array() {
		if item.Get("feature_name").String() == "image_gen" {
			found = true
			remaining = int(item.Get("remaining").Int())
			if raw := strings.TrimSpace(item.Get("reset_after").String()); raw != "" {
				if t, e := time.Parse(time.RFC3339, raw); e == nil {
					resetAt = &t
				}
			}
			break
		}
	}
	if !found {
		// Paid tiers sometimes omit image_gen in limits_progress. Do not treat as remaining=0.
		// Callers may still attempt generation; local cache stays "unknown" only if this errors.
		// Prefer a high remaining so strict policy does not hard-block after a successful probe.
		remaining = 999
	}
	return &Quota{Remaining: remaining, ResetAt: resetAt, ProbedAt: time.Now().UTC(), Raw: resp.String()}, nil
}

func stageLog(stage string, msg string) {
	if os.Getenv("WEBIMG_DEBUG") == "" {
		return
	}
	f, err := os.OpenFile("/tmp/webimg-stage.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		_, _ = fmt.Fprintf(f, "%s %s %s\n", time.Now().Format(time.RFC3339), stage, msg)
		_ = f.Close()
	}
}

func (d *Driver) Generate(ctx context.Context, auth Auth, req GenerateRequest) (*GenerateResult, error) {
	return d.run(ctx, auth, req, false)
}
func (d *Driver) Edit(ctx context.Context, auth Auth, req GenerateRequest) (*GenerateResult, error) {
	return d.run(ctx, auth, req, true)
}

func (d *Driver) run(ctx context.Context, auth Auth, genReq GenerateRequest, isEdit bool) (*GenerateResult, error) {
	start := time.Now()
	client, headers, err := d.session(auth)
	if err != nil {
		return nil, err
	}
	// Skip homepage warm-up GET: it costs a full RTT and is not required for backend-api calls.
	stageLog("requirements", "start")
	requirements, err := d.chatRequirements(ctx, client, headers)
	if err != nil {
		stageLog("requirements", err.Error())
		return nil, err
	}
	stageLog("requirements", "ok")
	// lightweight stage breadcrumb for ops logs
	_ = requirements
	var refs []map[string]any
	if isEdit && len(genReq.Images) == 0 {
		return nil, NewError(ErrorKindInternal, "edit", "edit requires at least one image", 0, false)
	}
	for i, img := range genReq.Images {
		name := img.FileName
		if name == "" {
			name = fmt.Sprintf("image_%d.png", i+1)
		}
		meta, err := d.uploadImage(ctx, client, headers, img, name)
		if err != nil {
			return nil, err
		}
		refs = append(refs, meta)
	}
	if genReq.Mask != nil {
		meta, err := d.uploadImage(ctx, client, headers, *genReq.Mask, "mask.png")
		if err != nil {
			return nil, err
		}
		refs = append(refs, meta)
	}
	model := imageModelSlug(genReq.Model)
	effort := normalizeThinkingEffort(genReq.ThinkingEffort)
	// Align with chatgpt2api build_image_prompt: force-generate + size/quality hints.
	finalPrompt := BuildImagePrompt(genReq.Prompt, genReq.Size, genReq.Quality, genReq.N)
	stageLog("prepare", "model="+model+" effort="+effort+" size="+strings.TrimSpace(genReq.Size)+" quality="+strings.TrimSpace(genReq.Quality))
	conduit, err := d.prepareConversation(ctx, client, headers, requirements, finalPrompt, model)
	if err != nil {
		stageLog("prepare", err.Error())
		return nil, err
	}
	stageLog("prepare", "conduit_len="+fmt.Sprintf("%d", len(conduit)))
	excludeInputs := inputAssetExcludeSet(refs)
	stageLog("sse", "start")
	conversationID, fileIDs, sedimentIDs, err := d.startConversation(ctx, client, headers, requirements, conduit, finalPrompt, model, effort, refs, excludeInputs)
	if err != nil {
		stageLog("sse", err.Error())
		return nil, err
	}
	fileIDs = filterAssetIDs(fileIDs, excludeInputs)
	sedimentIDs = filterAssetIDs(sedimentIDs, excludeInputs)
	stageLog("sse", fmt.Sprintf("cid=%s files=%d sediment=%d exclude_inputs=%d", conversationID, len(fileIDs), len(sedimentIDs), len(excludeInputs)))
	if conversationID == "" {
		return nil, NewError(ErrorKindUpstream, "sse", "missing conversation_id", 0, true)
	}
	// Hide conversation off the critical path so download → HTTP response is not blocked
	// by the PATCH is_visible=false RTT (often 1–3s; browser already showed the image).
	defer func(cid string) {
		go d.deleteConversationBestEffort(client, headers, cid)
	}(conversationID)
	// Edits: conversation JSON always contains the uploaded input pointer(s). Ignore those and
	// keep polling until a NEW generated asset appears (otherwise we "succeed" with the source image).
	if len(fileIDs) == 0 && len(sedimentIDs) == 0 {
		stageLog("poll", fmt.Sprintf("start cid=%s exclude_inputs=%d", conversationID, len(excludeInputs)))
		fileIDs, sedimentIDs, err = d.pollImages(ctx, client, headers, conversationID, excludeInputs)
		if err != nil {
			stageLog("poll", err.Error())
			return nil, err
		}
		stageLog("poll", fmt.Sprintf("files=%d sediment=%d", len(fileIDs), len(sedimentIDs)))
	}
	stageLog("download", fmt.Sprintf("files=%d sediment=%d", len(fileIDs), len(sedimentIDs)))
	limit := genReq.N
	if limit <= 0 {
		limit = 1
	}
	downloadStart := time.Now()
	blobs, err := d.downloadAssets(ctx, client, headers, conversationID, fileIDs, sedimentIDs, limit)
	if err != nil {
		stageLog("download", err.Error())
		return nil, err
	}
	stageLog("download", fmt.Sprintf("blobs=%d limit=%d took=%s total=%s", len(blobs), limit, time.Since(downloadStart).Round(time.Millisecond), time.Since(start).Round(time.Millisecond)))
	out := &GenerateResult{Created: time.Now().Unix(), Meta: Meta{ConversationID: conversationID, Stage: "done", Duration: time.Since(start)}}
	for _, b := range blobs {
		out.Data = append(out.Data, ImageData{B64JSON: base64.StdEncoding.EncodeToString(b)})
	}
	return out, nil
}

func (d *Driver) session(auth Auth) (*req.Client, map[string]string, error) {
	ua := strings.TrimSpace(auth.UserAgent)
	if ua == "" {
		ua = defaultUA
	}
	var client *req.Client
	var err error
	if d.ClientFactory != nil {
		client, err = d.ClientFactory(auth.ProxyURL)
		if err != nil {
			return nil, nil, NewError(ErrorKindTransport, "session", err.Error(), 0, true)
		}
	}
	if client == nil {
		client = req.C().ImpersonateChrome().SetTimeout(15 * time.Minute)
	}
	if auth.AccessToken == "" {
		return nil, nil, NewError(ErrorKindAuth, "session", "missing access token", 0, false)
	}
	headers := browserHeaders(ua)
	headers["Authorization"] = "Bearer " + auth.AccessToken
	if auth.DeviceID != "" {
		headers["Oai-Device-Id"] = auth.DeviceID
	} else {
		headers["Oai-Device-Id"] = uuid.NewString()
	}
	headers["OAI-Session-Id"] = uuid.NewString()
	headers["Oai-Language"] = "zh-CN"
	headers["Oai-Client-Version"] = "prod-de97061a1c9aff3931a7342defd6241031cd316a"
	headers["Oai-Client-Build-Number"] = "8160987"
	return client, headers, nil
}

func browserHeaders(ua string) map[string]string {
	if ua == "" {
		ua = defaultUA
	}
	return map[string]string{
		"User-Agent": ua, "Accept": "application/json, text/plain, */*", "Accept-Language": "en-US,en;q=0.9",
		"Origin": baseURL, "Referer": baseURL + "/",
		"Sec-Ch-Ua":        `"Chromium";v="131", "Not_A Brand";v="24", "Google Chrome";v="131"`,
		"Sec-Ch-Ua-Mobile": "?0", "Sec-Ch-Ua-Platform": `"Windows"`,
		"Sec-Fetch-Dest": "empty", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-origin",
	}
}

type chatRequirements struct{ Token, ProofToken string }

func (d *Driver) chatRequirements(ctx context.Context, client *req.Client, headers map[string]string) (*chatRequirements, error) {
	stage := "requirements"
	ua := headers["User-Agent"]
	pToken := buildRequirementsToken(ua)
	basePath := "/backend-api/sentinel/chat-requirements"
	prep, err := client.R().SetContext(ctx).SetHeaders(cloneHeaders(headers)).SetHeader("Content-Type", "application/json").
		SetHeader("X-OpenAI-Target-Path", basePath+"/prepare").SetHeader("X-OpenAI-Target-Route", basePath+"/prepare").
		SetBody(map[string]any{"p": pToken}).Post(baseURL + basePath + "/prepare")
	if err != nil {
		return nil, NewError(ErrorKindTransport, stage, err.Error(), 0, true)
	}
	if prep.StatusCode == http.StatusUnauthorized {
		return nil, NewError(ErrorKindAuth, stage, "token invalidated", prep.StatusCode, false)
	}
	if prep.StatusCode >= 400 {
		return nil, classifyHTTP(stage, prep.StatusCode, prep.String())
	}
	if gjson.GetBytes(prep.Bytes(), "arkose.required").Bool() {
		return nil, NewError(ErrorKindUpstream, stage, "arkose required", prep.StatusCode, false)
	}
	proofToken := ""
	if gjson.GetBytes(prep.Bytes(), "proofofwork.required").Bool() {
		proofToken, err = buildProofToken(gjson.GetBytes(prep.Bytes(), "proofofwork.seed").String(), gjson.GetBytes(prep.Bytes(), "proofofwork.difficulty").String(), ua)
		if err != nil {
			return nil, NewError(ErrorKindInternal, stage, err.Error(), 0, true)
		}
	}
	fin, err := client.R().SetContext(ctx).SetHeaders(cloneHeaders(headers)).SetHeader("Content-Type", "application/json").
		SetHeader("X-OpenAI-Target-Path", basePath+"/finalize").SetHeader("X-OpenAI-Target-Route", basePath+"/finalize").
		SetBody(map[string]any{"prepare_token": gjson.GetBytes(prep.Bytes(), "prepare_token").String(), "proof_token": proofToken, "turnstile_token": ""}).
		Post(baseURL + basePath + "/finalize")
	if err != nil {
		return nil, NewError(ErrorKindTransport, stage, err.Error(), 0, true)
	}
	if fin.StatusCode == http.StatusUnauthorized {
		return nil, NewError(ErrorKindAuth, stage, "token invalidated", fin.StatusCode, false)
	}
	if fin.StatusCode >= 400 {
		return nil, classifyHTTP(stage, fin.StatusCode, fin.String())
	}
	token := gjson.GetBytes(fin.Bytes(), "token").String()
	if token == "" {
		return nil, NewError(ErrorKindUpstream, stage, "missing requirements token", fin.StatusCode, true)
	}
	return &chatRequirements{Token: token, ProofToken: proofToken}, nil
}

func (d *Driver) uploadImage(ctx context.Context, client *req.Client, headers map[string]string, img InputImage, fallbackName string) (map[string]any, error) {
	stage := "upload"
	name := strings.TrimSpace(img.FileName)
	if name == "" {
		name = fallbackName
	}
	ct := strings.TrimSpace(img.ContentType)
	if ct == "" {
		ct = "image/png"
	}
	resp, err := client.R().SetContext(ctx).SetHeaders(cloneHeaders(headers)).SetHeader("Content-Type", "application/json").SetHeader("Accept", "application/json").
		SetHeader("X-OpenAI-Target-Path", "/backend-api/files").SetHeader("X-OpenAI-Target-Route", "/backend-api/files").
		SetBody(map[string]any{"file_name": name, "file_size": len(img.Data), "use_case": "multimodal", "width": 1024, "height": 1024}).
		Post(baseURL + "/backend-api/files")
	if err != nil {
		return nil, NewError(ErrorKindTransport, stage, err.Error(), 0, true)
	}
	if resp.StatusCode >= 400 {
		return nil, classifyHTTP(stage, resp.StatusCode, resp.String())
	}
	fileID := gjson.GetBytes(resp.Bytes(), "file_id").String()
	uploadURL := gjson.GetBytes(resp.Bytes(), "upload_url").String()
	if fileID == "" || uploadURL == "" {
		return nil, NewError(ErrorKindUpstream, stage, "missing upload meta", resp.StatusCode, true)
	}
	put, err := client.R().SetContext(ctx).SetHeader("Content-Type", ct).SetHeader("x-ms-blob-type", "BlockBlob").SetHeader("x-ms-version", "2020-04-08").
		SetHeader("Origin", baseURL).SetHeader("Referer", baseURL+"/").SetHeader("User-Agent", headers["User-Agent"]).SetBody(img.Data).Put(uploadURL)
	if err != nil {
		return nil, NewError(ErrorKindTransport, stage, err.Error(), 0, true)
	}
	if put.StatusCode >= 400 {
		return nil, classifyHTTP(stage, put.StatusCode, put.String())
	}
	path := "/backend-api/files/" + fileID + "/uploaded"
	done, err := client.R().SetContext(ctx).SetHeaders(cloneHeaders(headers)).SetHeader("Content-Type", "application/json").
		SetHeader("X-OpenAI-Target-Path", path).SetHeader("X-OpenAI-Target-Route", path).SetBodyString("{}").Post(baseURL + path)
	if err != nil {
		return nil, NewError(ErrorKindTransport, stage, err.Error(), 0, true)
	}
	if done.StatusCode >= 400 {
		return nil, classifyHTTP(stage, done.StatusCode, done.String())
	}
	return map[string]any{"file_id": fileID, "file_name": name, "file_size": len(img.Data), "mime_type": ct, "width": 1024, "height": 1024}, nil
}

func (d *Driver) prepareConversation(ctx context.Context, client *req.Client, headers map[string]string, reqs *chatRequirements, prompt, model string) (string, error) {
	stage := "prepare"
	path := "/backend-api/f/conversation/prepare"
	// Align with chatgpt.com capture (Tools → Create image): empty system_hints, no picture_v2.
	payload := map[string]any{
		"action": "next", "fork_from_shared_post": false, "parent_message_id": "client-created-root", "model": model,
		"client_prepare_state": "none", "timezone_offset_min": -480, "timezone": "Asia/Shanghai",
		"conversation_mode": map[string]any{"kind": "primary_assistant"}, "system_hints": []any{},
		"partial_query": map[string]any{
			"id": uuid.NewString(), "author": map[string]any{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []string{prompt}},
		},
		"supports_buffering": true, "supported_encodings": []string{"v1"},
	}
	h := cloneHeaders(headers)
	h["Content-Type"] = "application/json"
	h["X-Conduit-Token"] = "no-token"
	h["OpenAI-Sentinel-Chat-Requirements-Token"] = reqs.Token
	if reqs.ProofToken != "" {
		h["OpenAI-Sentinel-Proof-Token"] = reqs.ProofToken
	}
	h["X-OpenAI-Target-Path"] = path
	h["X-OpenAI-Target-Route"] = path
	resp, err := client.R().SetContext(ctx).SetHeaders(h).SetBody(payload).Post(baseURL + path)
	if err != nil {
		return "", NewError(ErrorKindTransport, stage, err.Error(), 0, true)
	}
	if resp.StatusCode >= 400 {
		return "", classifyHTTP(stage, resp.StatusCode, resp.String())
	}
	return gjson.GetBytes(resp.Bytes(), "conduit_token").String(), nil
}

func (d *Driver) startConversation(ctx context.Context, client *req.Client, headers map[string]string, reqs *chatRequirements, conduitToken, prompt, model, thinkingEffort string, refs []map[string]any, excludeInputs map[string]struct{}) (string, []string, []string, error) {
	stage := "sse"
	parts := make([]any, 0, len(refs)+1)
	attachments := make([]map[string]any, 0, len(refs))
	for _, item := range refs {
		fileID, _ := item["file_id"].(string)
		parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + fileID, "width": item["width"], "height": item["height"], "size_bytes": item["file_size"]})
		attachments = append(attachments, map[string]any{"id": fileID, "mimeType": item["mime_type"], "name": item["file_name"], "size": item["file_size"], "width": item["width"], "height": item["height"]})
	}
	parts = append(parts, prompt)
	content := map[string]any{"content_type": "text", "parts": []any{prompt}}
	if len(refs) > 0 {
		content = map[string]any{"content_type": "multimodal_text", "parts": parts}
	}
	// Official web Tools→Create image capture (2026-07-13):
	// model=gpt-5-6-thinking, system_hints=[], thinking_effort=extended, client_prepare_state=none.
	// Using picture_v2 + gpt-5-3 often creates a conversation that never actually generates images.
	metadata := map[string]any{
		"selected_connector_ids": []any{},
		"selected_sources":       []any{},
		"serialization_metadata": map[string]any{"custom_symbol_offsets": []any{}},
	}
	if len(attachments) > 0 {
		metadata["attachments"] = attachments
	}
	now := float64(time.Now().UnixNano()) / 1e9
	payload := map[string]any{
		"action":            "next",
		"messages":          []map[string]any{{"id": uuid.NewString(), "author": map[string]any{"role": "user"}, "create_time": now, "content": content, "metadata": metadata}},
		"parent_message_id": "client-created-root", "model": model, "client_prepare_state": "none", "timezone_offset_min": -480, "timezone": "Asia/Shanghai",
		"conversation_mode": map[string]any{"kind": "primary_assistant"}, "enable_message_followups": true, "system_hints": []any{},
		"supports_buffering": true, "supported_encodings": []string{"v1"},
		"client_contextual_info": map[string]any{
			"is_dark_mode": false, "time_since_loaded": 16, "page_height": 851, "page_width": 1442, "pixel_ratio": 2,
			"screen_height": 1080, "screen_width": 1920, "app_name": "chatgpt.com",
			"has_web_push_capabilities": true, "web_push_notification_permission": "default",
		},
		"paragen_cot_summary_display_override": "allow", "force_parallel_switch": "auto",
		"thinking_effort": thinkingEffort,
	}
	path := "/backend-api/f/conversation"
	h := cloneHeaders(headers)
	h["Content-Type"] = "application/json"
	h["Accept"] = "text/event-stream"
	h["OpenAI-Sentinel-Chat-Requirements-Token"] = reqs.Token
	if reqs.ProofToken != "" {
		h["OpenAI-Sentinel-Proof-Token"] = reqs.ProofToken
	}
	if conduitToken != "" {
		h["X-Conduit-Token"] = conduitToken
	} else {
		h["X-Conduit-Token"] = "no-token"
	}
	h["X-Oai-Turn-Trace-Id"] = uuid.NewString()
	h["X-OpenAI-Target-Path"] = path
	h["X-OpenAI-Target-Route"] = path
	resp, err := client.R().SetContext(ctx).SetHeaders(h).SetBody(payload).DisableAutoReadResponse().Post(baseURL + path)
	if err != nil {
		return "", nil, nil, NewError(ErrorKindTransport, stage, err.Error(), 0, true)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", nil, nil, NewError(ErrorKindAuth, stage, "token invalidated", resp.StatusCode, false)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", nil, nil, classifyHTTP(stage, resp.StatusCode, string(b))
	}
	var conversationID string
	var fileIDs, sedimentIDs []string
	appendFile := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, skip := excludeInputs[normalizeAssetID(id)]; skip {
			return
		}
		fileIDs = appendUnique(fileIDs, id)
	}
	appendSediment := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, skip := excludeInputs[normalizeAssetID(id)]; skip {
			return
		}
		sedimentIDs = appendUnique(sedimentIDs, id)
	}

	// Read SSE off the main path so idle/max timeouts can break hung streams.
	// Official web keeps the stream open after useful image events; hanging here
	// makes account tests look stuck for minutes with only heartbeats.
	lines := make(chan string, 64)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		if err := sc.Err(); err != nil {
			readErr <- err
		}
		close(lines)
	}()

	// Architecture (strict):
	//   1) While SSE is open: ONLY read SSE events — zero GET /conversation polls.
	//   2) When SSE ends / idles / [DONE] without assets: hand off to pollImages().
	// This matches production observation that concurrent soft-poll + SSE causes 429s
	// even though the image already exists in the web UI.
	sseDeadline := time.Now().Add(defaultSSEMaxWait)
loop:
	for len(fileIDs) == 0 && len(sedimentIDs) == 0 {
		remaining := time.Until(sseDeadline)
		if remaining <= 0 {
			stageLog("sse", "max_wait_reached")
			break
		}
		idleWait := defaultSSEIdleWait
		if conversationID == "" {
			idleWait = defaultSSEIdleWaitNoCID
		}
		if idleWait > remaining {
			idleWait = remaining
		}
		idleTimer := time.NewTimer(idleWait)

		select {
		case <-ctx.Done():
			idleTimer.Stop()
			if conversationID == "" {
				return "", nil, nil, NewError(ErrorKindTimeout, stage, ctx.Err().Error(), 0, true)
			}
			stageLog("sse", "ctx_done_leave_to_poll")
			break loop
		case err := <-readErr:
			idleTimer.Stop()
			if conversationID == "" {
				return "", nil, nil, NewError(ErrorKindTransport, stage, err.Error(), 0, true)
			}
			stageLog("sse", "stream_read_end_leave_to_poll")
			break loop
		case raw, ok := <-lines:
			idleTimer.Stop()
			if !ok {
				stageLog("sse", "stream_closed_leave_to_poll")
				break loop
			}
			if !strings.HasPrefix(raw, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(raw, "data:"))
			if data == "" {
				continue
			}
			if data == "[DONE]" {
				if len(fileIDs) > 0 || len(sedimentIDs) > 0 {
					stageLog("sse", "done_event_with_assets")
					break loop
				}
				// Drain a short tail of SSE (still NO conversation GET). Late events sometimes
				// carry file pointers right after [DONE]; then hand off to post-SSE poll.
				stageLog("sse", "done_without_assets_drain")
				drainDeadline := time.Now().Add(defaultSSEDrainAfterDone)
			drainLoop:
				for time.Now().Before(drainDeadline) {
					if len(fileIDs) > 0 || len(sedimentIDs) > 0 {
						break drainLoop
					}
					rem := time.Until(drainDeadline)
					if rem <= 0 {
						break
					}
					drainTimer := time.NewTimer(rem)
					select {
					case <-ctx.Done():
						drainTimer.Stop()
						break drainLoop
					case err := <-readErr:
						drainTimer.Stop()
						_ = err
						break drainLoop
					case raw2, ok2 := <-lines:
						drainTimer.Stop()
						if !ok2 {
							break drainLoop
						}
						if !strings.HasPrefix(raw2, "data:") {
							continue
						}
						d2 := strings.TrimSpace(strings.TrimPrefix(raw2, "data:"))
						if d2 == "" || d2 == "[DONE]" {
							continue
						}
						if cid := gjson.Get(d2, "conversation_id").String(); cid != "" {
							conversationID = cid
						}
						for _, p := range extractPointers([]byte(d2)) {
							if strings.HasPrefix(p, "file-service://") {
								appendFile(strings.TrimPrefix(p, "file-service://"))
							} else if strings.HasPrefix(p, "sediment://") {
								appendSediment(strings.TrimPrefix(p, "sediment://"))
							}
						}
						if rl := extractTaskRateLimitError([]byte(d2)); rl != "" {
							return conversationID, fileIDs, sedimentIDs, NewError(ErrorKindRateLimited, stage, rl, http.StatusTooManyRequests, true)
						}
						if policy := extractTaskPolicyError([]byte(d2)); policy != "" {
							return conversationID, fileIDs, sedimentIDs, NewError(ErrorKindPolicy, stage, policy, http.StatusBadRequest, false)
						}
					case <-drainTimer.C:
						break drainLoop
					}
				}
				if len(fileIDs) > 0 || len(sedimentIDs) > 0 {
					stageLog("sse", fmt.Sprintf("asset_from_sse_drain files=%d sediment=%d", len(fileIDs), len(sedimentIDs)))
				} else {
					stageLog("sse", "done_drain_done_leave_to_poll")
				}
				break loop
			}
			if cid := gjson.Get(data, "conversation_id").String(); cid != "" {
				if conversationID == "" {
					conversationID = cid
					stageLog("sse", "cid="+cid)
				} else {
					conversationID = cid
				}
			}
			for _, p := range extractPointers([]byte(data)) {
				if strings.HasPrefix(p, "file-service://") {
					appendFile(strings.TrimPrefix(p, "file-service://"))
				} else if strings.HasPrefix(p, "sediment://") {
					appendSediment(strings.TrimPrefix(p, "sediment://"))
				}
			}
			if len(fileIDs) > 0 || len(sedimentIDs) > 0 {
				stageLog("sse", fmt.Sprintf("asset_from_sse files=%d sediment=%d", len(fileIDs), len(sedimentIDs)))
				break loop
			}
			if rl := extractTaskRateLimitError([]byte(data)); rl != "" {
				return conversationID, fileIDs, sedimentIDs, NewError(ErrorKindRateLimited, stage, rl, http.StatusTooManyRequests, true)
			}
			if policy := extractTaskPolicyError([]byte(data)); policy != "" {
				return conversationID, fileIDs, sedimentIDs, NewError(ErrorKindPolicy, stage, policy, http.StatusBadRequest, false)
			}
		case <-idleTimer.C:
			// Stream quiet. No polling here — leave SSE phase and let pollImages run.
			if conversationID != "" {
				stageLog("sse", "idle_leave_to_poll cid="+conversationID)
				break loop
			}
			if time.Now().After(sseDeadline) {
				break loop
			}
		}
	}
	if conversationID == "" {
		return "", nil, nil, NewError(ErrorKindUpstream, stage, "missing conversation_id", 0, true)
	}
	// Empty assets → run() calls pollImages() only after SSE phase ends.
	return conversationID, fileIDs, sedimentIDs, nil
}

func (d *Driver) pollImages(ctx context.Context, client *req.Client, headers map[string]string, conversationID string, excludeInputs map[string]struct{}) ([]string, []string, error) {
	stage := "poll"
	timeout := d.PollTimeout
	if timeout <= 0 {
		timeout = defaultPollTimeout
	}
	baseEvery := d.PollInterval
	if baseEvery <= 0 {
		baseEvery = defaultPollEvery
	}
	started := time.Now()
	deadline := started.Add(timeout)

	// Elapsed-based adaptive poll + dedicated 429 backoff (see poll_schedule.go).
	// Immediate first GET (no pre-wait); then ~4s cadence (see poll_schedule.go).
	attempt := 0
	consecutive429 := 0
	for time.Now().Before(deadline) {
		attempt++
		elapsed := time.Since(started)

		path := "/backend-api/conversation/" + conversationID
		pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := client.R().SetContext(pctx).SetHeaders(cloneHeaders(headers)).SetHeader("Accept", "application/json").
			SetHeader("X-OpenAI-Target-Path", path).
			SetHeader("X-OpenAI-Target-Route", "/backend-api/conversation/{conversation_id}").
			Get(baseURL + path)
		pcancel()
		if err != nil {
			wait := pollIntervalForElapsed(elapsed, baseEvery)
			stageLog("poll", fmt.Sprintf("transport attempt=%d wait=%s err=%s", attempt, wait, err.Error()))
			select {
			case <-ctx.Done():
				return nil, nil, NewError(ErrorKindTimeout, stage, ctx.Err().Error(), 0, true)
			case <-time.After(wait):
			}
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			consecutive429++
			// Keep retrying until overall poll deadline — image may already exist upstream.
			wait := pollBackoffAfter429(consecutive429)
			stageLog("poll", fmt.Sprintf("429 attempt=%d consecutive=%d backoff=%s (continue until deadline)", attempt, consecutive429, wait))
			select {
			case <-ctx.Done():
				return nil, nil, NewError(ErrorKindTimeout, stage, ctx.Err().Error(), 0, true)
			case <-time.After(wait):
			}
			continue
		}
		if resp.StatusCode >= 500 {
			wait := pollIntervalForElapsed(elapsed, baseEvery)
			if wait < time.Second {
				wait = time.Second
			}
			stageLog("poll", fmt.Sprintf("5xx attempt=%d status=%d wait=%s", attempt, resp.StatusCode, wait))
			select {
			case <-ctx.Done():
				return nil, nil, NewError(ErrorKindTimeout, stage, ctx.Err().Error(), 0, true)
			case <-time.After(wait):
			}
			continue
		}
		if resp.StatusCode >= 400 {
			bodyText := resp.String()
			// Early/mid polls can race before conversation ACL is fully visible (common right
			// after SSE starts, especially on edits with attachments).
			inaccessible := resp.StatusCode == http.StatusNotFound ||
				strings.Contains(strings.ToLower(bodyText), "conversation_inaccessible") ||
				strings.Contains(bodyText, "无权访问此对话")
			if inaccessible && elapsed < 60*time.Second {
				stageLog("poll", fmt.Sprintf("inaccessible attempt=%d elapsed=%s retry", attempt, elapsed.Round(time.Millisecond)))
				select {
				case <-ctx.Done():
					return nil, nil, NewError(ErrorKindTimeout, stage, ctx.Err().Error(), 0, true)
				case <-time.After(2 * time.Second):
				}
				continue
			}
			return nil, nil, classifyHTTP(stage, resp.StatusCode, bodyText)
		}
		consecutive429 = 0 // healthy response resets 429 streak
		body := resp.Bytes()
		var fileIDs, sedimentIDs []string
		for _, p := range extractPointers(body) {
			if strings.HasPrefix(p, "file-service://") {
				id := strings.TrimPrefix(p, "file-service://")
				if _, skip := excludeInputs[normalizeAssetID(id)]; !skip {
					fileIDs = appendUnique(fileIDs, id)
				}
			} else if strings.HasPrefix(p, "sediment://") {
				id := strings.TrimPrefix(p, "sediment://")
				if _, skip := excludeInputs[normalizeAssetID(id)]; !skip {
					sedimentIDs = appendUnique(sedimentIDs, id)
				}
			}
		}
		if len(fileIDs) > 0 || len(sedimentIDs) > 0 {
			stageLog("poll", fmt.Sprintf("found attempt=%d elapsed=%s files=%d sediment=%d", attempt, elapsed.Round(time.Millisecond), len(fileIDs), len(sedimentIDs)))
			return fileIDs, sedimentIDs, nil
		}
		if attempt == 1 || attempt%8 == 0 {
			stageLog("poll", fmt.Sprintf("waiting attempt=%d elapsed=%s", attempt, elapsed.Round(time.Millisecond)))
		}
		if rl := findConversationRateLimitError(body); rl != "" {
			return nil, nil, NewError(ErrorKindRateLimited, stage, rl, http.StatusTooManyRequests, true)
		}
		if policy := findConversationPolicyError(body); policy != "" {
			return nil, nil, NewError(ErrorKindPolicy, stage, policy, http.StatusBadRequest, false)
		}

		wait := pollIntervalForElapsed(elapsed, baseEvery)
		select {
		case <-ctx.Done():
			return nil, nil, NewError(ErrorKindTimeout, stage, ctx.Err().Error(), 0, true)
		case <-time.After(wait):
		}
	}
	return nil, nil, NewError(ErrorKindTimeout, stage, "image poll timeout", 0, true)
}

func inputAssetExcludeSet(refs []map[string]any) map[string]struct{} {
	out := make(map[string]struct{}, len(refs))
	for _, item := range refs {
		if item == nil {
			continue
		}
		id, _ := item["file_id"].(string)
		id = normalizeAssetID(id)
		if id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

func filterAssetIDs(ids []string, exclude map[string]struct{}) []string {
	if len(ids) == 0 || len(exclude) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		nid := normalizeAssetID(id)
		if nid == "" {
			continue
		}
		if _, skip := exclude[nid]; skip {
			continue
		}
		out = append(out, id)
	}
	return out
}

func normalizeAssetID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "file-service://")
	id = strings.TrimPrefix(id, "sediment://")
	return strings.TrimSpace(id)
}

type downloadCandidate struct {
	ID       string
	Sediment bool
}

// mergeDownloadCandidates de-duplicates file-service and sediment pointers that often
// reference the same generated image (previously caused duplicate identical outputs).
func mergeDownloadCandidates(fileIDs, sedimentIDs []string) []downloadCandidate {
	seen := make(map[string]struct{}, len(fileIDs)+len(sedimentIDs))
	out := make([]downloadCandidate, 0, len(fileIDs)+len(sedimentIDs))
	add := func(raw string, sediment bool) {
		id := normalizeAssetID(raw)
		if id == "" || id == "file_upload" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		// file_000... under sediment:// still prefers the file download route.
		if strings.HasPrefix(id, "file_00000000") || strings.HasPrefix(id, "file-") {
			sediment = false
		}
		out = append(out, downloadCandidate{ID: id, Sediment: sediment})
	}
	for _, id := range fileIDs {
		add(id, false)
	}
	for _, id := range sedimentIDs {
		add(id, true)
	}
	return out
}

func (d *Driver) downloadAssets(ctx context.Context, client *req.Client, headers map[string]string, conversationID string, fileIDs, sedimentIDs []string, limit int) ([][]byte, error) {
	// No fixed pre-wait: file links are usually ready when conversation pointers appear.
	// downloadOneWithRetry handles transient "not ready" with short backoff.
	if limit <= 0 {
		limit = 1
	}
	candidates := mergeDownloadCandidates(fileIDs, sedimentIDs)
	var out [][]byte
	var lastErr error
	seenHash := make(map[string]struct{}, limit)
	for _, c := range candidates {
		if len(out) >= limit {
			break
		}
		b, err := d.downloadOneWithRetry(ctx, client, headers, conversationID, c.ID, c.Sediment)
		if err != nil {
			lastErr = err
			continue
		}
		if len(b) == 0 {
			continue
		}
		sum := sha256.Sum256(b)
		key := hex.EncodeToString(sum[:])
		if _, ok := seenHash[key]; ok {
			continue
		}
		seenHash[key] = struct{}{}
		out = append(out, b)
	}
	if len(out) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, NewError(ErrorKindUpstream, "download", "empty downloads", 0, true)
	}
	return out, nil
}

func (d *Driver) downloadOneWithRetry(ctx context.Context, client *req.Client, headers map[string]string, conversationID, id string, sediment bool) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			// 120ms, 240ms, 360ms... — file links often appear within ~1s of conversation pointers.
			select {
			case <-ctx.Done():
				return nil, NewError(ErrorKindTimeout, "download", ctx.Err().Error(), 0, true)
			case <-time.After(time.Duration(attempt) * 120 * time.Millisecond):
			}
		}
		b, err := d.downloadOne(ctx, client, headers, conversationID, id, sediment)
		if err == nil {
			return b, nil
		}
		lastErr = err
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "file link not found") && !strings.Contains(msg, "missing download url") && !strings.Contains(msg, "not found") {
			return nil, err
		}
	}
	return nil, lastErr
}

func (d *Driver) downloadOne(ctx context.Context, client *req.Client, headers map[string]string, conversationID, id string, sediment bool) ([]byte, error) {
	stage := "download"
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, NewError(ErrorKindInternal, stage, "empty asset id", 0, false)
	}

	resolve := func(resp *req.Response) string {
		if resp == nil {
			return ""
		}
		u := gjson.GetBytes(resp.Bytes(), "download_url").String()
		if u == "" {
			u = gjson.GetBytes(resp.Bytes(), "url").String()
		}
		return u
	}

	var downloadURL string
	var lastStatus int
	var lastBody string

	// Official web path captured 2026-07-13 from chatgpt.com:
	// GET /backend-api/files/download/{file_id}?conversation_id=...&inline=false&download_intent=false
	// Response: {"status":"success","download_url":"https://chatgpt.com/backend-api/estuary/content?..."}
	// Old path /backend-api/files/{id}/download returns {"detail":"File link not found."}.
	tryFile := !sediment || strings.HasPrefix(id, "file_00000000") || strings.HasPrefix(id, "file-")
	if tryFile {
		path := "/backend-api/files/download/" + id
		r := client.R().SetContext(ctx).SetHeaders(cloneHeaders(headers)).SetHeader("Accept", "application/json").
			SetHeader("X-OpenAI-Target-Path", path).
			SetHeader("X-OpenAI-Target-Route", "/backend-api/files/download/{file_id}").
			SetQueryParam("inline", "false").
			SetQueryParam("download_intent", "false")
		if conversationID != "" {
			r.SetQueryParam("conversation_id", conversationID)
		}
		resp, err := r.Get(baseURL + path)
		if err != nil {
			return nil, NewError(ErrorKindTransport, stage, err.Error(), 0, true)
		}
		lastStatus, lastBody = resp.StatusCode, resp.String()
		if resp.StatusCode < 400 {
			downloadURL = resolve(resp)
		}
		// Legacy fallback.
		if downloadURL == "" {
			legacy := "/backend-api/files/" + id + "/download"
			resp2, err2 := client.R().SetContext(ctx).SetHeaders(cloneHeaders(headers)).SetHeader("Accept", "application/json").
				SetHeader("X-OpenAI-Target-Path", legacy).SetHeader("X-OpenAI-Target-Route", legacy).Get(baseURL + legacy)
			if err2 == nil {
				lastStatus, lastBody = resp2.StatusCode, resp2.String()
				if resp2.StatusCode < 400 {
					downloadURL = resolve(resp2)
				}
			}
		}
	}

	// Conversation attachment download for sediment ids.
	if downloadURL == "" && conversationID != "" {
		path := "/backend-api/conversation/" + conversationID + "/attachment/" + id + "/download"
		resp, err := client.R().SetContext(ctx).SetHeaders(cloneHeaders(headers)).SetHeader("Accept", "application/json").
			SetHeader("X-OpenAI-Target-Path", path).
			SetHeader("X-OpenAI-Target-Route", "/backend-api/conversation/{conversation_id}/attachment/{attachment_id}/download").
			Get(baseURL + path)
		if err != nil {
			return nil, NewError(ErrorKindTransport, stage, err.Error(), 0, true)
		}
		lastStatus, lastBody = resp.StatusCode, resp.String()
		if resp.StatusCode < 400 {
			downloadURL = resolve(resp)
		}
	}

	// Legacy query form.
	if downloadURL == "" && conversationID != "" {
		for _, pointer := range []string{"file-service://" + id, "sediment://" + id, id} {
			path := "/backend-api/conversation/" + conversationID + "/attachment"
			resp, err := client.R().SetContext(ctx).SetHeaders(cloneHeaders(headers)).SetHeader("Accept", "application/json").
				SetHeader("X-OpenAI-Target-Path", path).SetHeader("X-OpenAI-Target-Route", path).
				SetQueryParam("asset_pointer", pointer).Get(baseURL + path)
			if err != nil {
				continue
			}
			lastStatus, lastBody = resp.StatusCode, resp.String()
			if resp.StatusCode < 400 {
				downloadURL = resolve(resp)
				if downloadURL != "" {
					break
				}
			}
		}
	}

	if downloadURL == "" {
		if lastBody != "" {
			return nil, classifyHTTP(stage, lastStatus, lastBody)
		}
		return nil, NewError(ErrorKindUpstream, stage, "missing download url", lastStatus, true)
	}

	// estuary/content is on chatgpt.com and needs auth headers.
	imgReq := client.R().SetContext(ctx)
	if strings.Contains(downloadURL, "chatgpt.com") {
		imgReq.SetHeaders(cloneHeaders(headers))
	}
	img, err := imgReq.Get(downloadURL)
	if err != nil {
		return nil, NewError(ErrorKindTransport, stage, err.Error(), 0, true)
	}
	if img.StatusCode >= 400 {
		return nil, classifyHTTP(stage, img.StatusCode, img.String())
	}
	if len(img.Bytes()) == 0 {
		return nil, NewError(ErrorKindUpstream, stage, "empty image body", img.StatusCode, true)
	}
	return img.Bytes(), nil
}

// deleteConversationBestEffort hides the conversation in ChatGPT history (PATCH is_visible=false).
// Failures are ignored so image success/failure is never blocked by cleanup.
func (d *Driver) deleteConversationBestEffort(client *req.Client, headers map[string]string, conversationID string) {
	if d != nil && d.KeepConversation {
		return
	}
	conversationID = strings.TrimSpace(conversationID)
	if client == nil || conversationID == "" {
		return
	}
	path := "/backend-api/conversation/" + conversationID
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	h := cloneHeaders(headers)
	h["Accept"] = "*/*"
	h["Content-Type"] = "application/json"
	h["Referer"] = baseURL + "/c/" + conversationID
	h["X-OpenAI-Target-Path"] = path
	h["X-OpenAI-Target-Route"] = "/backend-api/conversation/{conversation_id}"
	_, err := client.R().SetContext(ctx).SetHeaders(h).SetBody(map[string]any{"is_visible": false}).Patch(baseURL + path)
	if err != nil {
		stageLog("cleanup", "delete conversation failed: "+err.Error())
		return
	}
	stageLog("cleanup", "conversation hidden "+conversationID)
}

func imageModelSlug(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	// Browser Create-image path uses the thinking chat model, not a dedicated gpt-image slug.
	if m == "" || m == "auto" || m == "gpt-image-2" || m == "gpt-image-1" || strings.Contains(m, "gpt-image") {
		return "gpt-5-6-thinking"
	}
	// Already an upstream ChatGPT model slug (from resolver / admin fixed config).
	if strings.HasPrefix(m, "gpt-") {
		return strings.TrimSpace(model)
	}
	return "gpt-5-6-thinking"
}

func normalizeThinkingEffort(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "none", "minimal", "low", "standard", "medium", "high", "extended", "max", "pro", "xhigh":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return "extended"
	}
}

func classifyHTTP(stage string, status int, body string) *Error {
	if status == http.StatusUnauthorized {
		return NewError(ErrorKindAuth, stage, "token invalidated", status, false)
	}
	if looksLikeRateLimitMessage(body) && IsImageQuotaLimitedMessage(body) {
		return NewError(ErrorKindRateLimited, stage, truncate(body, 500), statusOr(status, http.StatusTooManyRequests), true)
	}
	if status == http.StatusTooManyRequests {
		// HTTP 429 without quota phrasing = temporary throttle (conversation/read), not image quota.
		return NewError(ErrorKindTransport, stage, truncate(body, 500), http.StatusTooManyRequests, true)
	}
	if looksLikePolicyMessage(body) {
		return NewError(ErrorKindPolicy, stage, truncate(body, 300), status, false)
	}
	return NewError(ErrorKindUpstream, stage, truncate(body, 300), status, status >= 500)
}

func statusOr(status, fallback int) int {
	if status > 0 {
		return status
	}
	return fallback
}

// looksLikeRateLimitMessage detects Free/Plus image quota exhaustion text from ChatGPT web.
// These must take priority over policy heuristics (which previously swallowed "can't generate").
// IsImageQuotaLimitedMessage reports ChatGPT *image generation quota* limits
// (Free plan image caps, resets in N hours, etc.). HTTP 429 on conversation GET
// is NOT quota and must not mark the account web-image cooldown / remaining=0.
func IsImageQuotaLimitedMessage(text string) bool {
	msg := strings.TrimSpace(text)
	if msg == "" {
		return false
	}
	l := strings.ToLower(msg)
	// Explicit non-quota read/throttle paths.
	if strings.Contains(l, "soft poll 429") ||
		strings.Contains(l, "conversation poll rate limited") ||
		strings.Contains(l, "conversation get 429") {
		return false
	}
	// Prefer clear image-quota phrasing over generic "rate limit".
	keys := []string{
		"free plan limit",
		"image generation limit",
		"image generations",
		"limit for image",
		"you've hit the",
		"you have hit the",
		"resets in",
		"limit resets",
		"免费版额度",
		"免费计划",
		"生成次数已达",
		"图片生成",
		"生图",
	}
	for _, k := range keys {
		if strings.Contains(l, k) || strings.Contains(msg, k) {
			return true
		}
	}
	// Generic rate-limit text without image context: treat as non-quota throttle.
	return false
}

func looksLikeRateLimitMessage(text string) bool {
	l := strings.ToLower(strings.TrimSpace(text))
	if l == "" {
		return false
	}
	keywords := []string{
		"free plan limit",
		"plus plan limit",
		"pro plan limit",
		"plan limit for image",
		"image generation limit",
		"image generations requests",
		"limit resets",
		"rate limit",
		"rate_limit",
		"usage_limit",
		"usage limit",
		"too many requests",
		"hit the limit",
		"you've hit the",
		"you have hit the",
		"reached the free plan",
		"reached the limit",
		"quota exceeded",
		"out of image",
		"no remaining",
		"remaining images",
		"limit for image generations",
		"upgrade to a plan",
		"额度用尽",
		"次数已用完",
		"达到上限",
		"用量上限",
		"限流",
		"免费版额度",
		"免费计划",
		"生成次数已达",
		"请等待重置",
		"重置后",
	}
	for _, k := range keywords {
		if strings.Contains(l, k) || strings.Contains(text, k) {
			return true
		}
	}
	return false
}

func looksLikePolicyMessage(text string) bool {
	l := strings.ToLower(strings.TrimSpace(text))
	if l == "" {
		return false
	}
	// Rate-limit text often includes "can't generate" — never classify those as policy.
	if looksLikeRateLimitMessage(text) {
		return false
	}
	// Only match explicit refusal/policy phrasing on message text (not whole conversation JSON).
	keywords := []string{
		"content policy",
		"content_policy",
		"violat",
		"moderation",
		"not allowed",
		"i can't help",
		"i cannot help",
		"can't generate",
		"cannot generate",
		"unable to generate",
		"blocked",
		"内容政策",
		"防护限制",
		"不能生成",
		"无法生成",
		"不能帮助",
		"无法帮助",
		"抱歉，我不能",
	}
	for _, k := range keywords {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

func extractPointers(body []byte) []string {
	raw := string(body)
	var out []string
	for _, prefix := range []string{"file-service://", "sediment://"} {
		start := 0
		for {
			i := strings.Index(raw[start:], prefix)
			if i < 0 {
				break
			}
			i += start
			end := i + len(prefix)
			for end < len(raw) {
				ch := raw[end]
				isIDChar := ch == '-' || ch == '_' || (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
				if !isIDChar {
					break
				}
				end++
			}
			out = append(out, raw[i:end])
			start = end
		}
	}
	// Real image file ids often appear as file_00000000 + 24 hex chars.
	const marker = "file_00000000"
	start := 0
	for {
		i := strings.Index(raw[start:], marker)
		if i < 0 {
			break
		}
		i += start
		end := i + len(marker)
		hexCount := 0
		for end < len(raw) && hexCount < 24 {
			ch := raw[end]
			if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F') {
				end++
				hexCount++
				continue
			}
			break
		}
		if hexCount == 24 {
			out = appendUnique(out, "file-service://"+raw[i:end])
		}
		start = i + len(marker)
	}
	return out
}

// extractTaskPolicyError only trusts explicit error payloads, never whole-body keyword scans.
func extractTaskPolicyError(body []byte) string {
	for _, path := range []string{
		"items.0.error.message",
		"0.error.message",
		"error.message",
		"detail",
	} {
		msg := strings.TrimSpace(gjson.GetBytes(body, path).String())
		if msg != "" && looksLikePolicyMessage(msg) {
			return msg
		}
	}
	return ""
}

func extractTaskRateLimitError(body []byte) string {
	for _, path := range []string{
		"items.0.error.message",
		"0.error.message",
		"error.message",
		"detail",
		"message",
	} {
		msg := strings.TrimSpace(gjson.GetBytes(body, path).String())
		if msg != "" && looksLikeRateLimitMessage(msg) {
			return msg
		}
	}
	// SSE event bodies may embed limit text without structured error fields.
	if looksLikeRateLimitMessage(string(body)) {
		return truncate(string(body), 500)
	}
	return ""
}

// findConversationRateLimitError inspects assistant/tool texts for image quota exhaustion.
func findConversationRateLimitError(body []byte) string {
	return scanConversationAssistantText(body, looksLikeRateLimitMessage)
}

// findConversationPolicyError inspects assistant/tool message texts only.
func findConversationPolicyError(body []byte) string {
	return scanConversationAssistantText(body, looksLikePolicyMessage)
}

func scanConversationAssistantText(body []byte, match func(string) bool) string {
	if match == nil {
		return ""
	}
	mapping := gjson.GetBytes(body, "mapping")
	if !mapping.Exists() || !mapping.IsObject() {
		if s := strings.TrimSpace(string(body)); s != "" && match(s) {
			if len(s) > 500 {
				s = s[:500]
			}
			return s
		}
		return ""
	}
	var hit string
	mapping.ForEach(func(_, node gjson.Result) bool {
		msg := node.Get("message")
		if !msg.Exists() {
			return true
		}
		role := strings.ToLower(strings.TrimSpace(msg.Get("author.role").String()))
		if role != "assistant" && role != "tool" {
			return true
		}
		var texts []string
		content := msg.Get("content")
		if content.IsObject() {
			if parts := content.Get("parts"); parts.IsArray() {
				parts.ForEach(func(_, part gjson.Result) bool {
					if part.Type == gjson.String {
						if s := strings.TrimSpace(part.String()); s != "" {
							texts = append(texts, s)
						}
					}
					return true
				})
			}
			if s := strings.TrimSpace(content.Get("text").String()); s != "" {
				texts = append(texts, s)
			}
		} else if content.Type == gjson.String {
			if s := strings.TrimSpace(content.String()); s != "" {
				texts = append(texts, s)
			}
		}
		joined := strings.Join(texts, "\n")
		if match(joined) {
			if len(joined) > 500 {
				joined = joined[:500]
			}
			hit = joined
			return false
		}
		return true
	})
	return hit
}

func appendUnique(items []string, v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return items
	}
	for _, x := range items {
		if x == v {
			return items
		}
	}
	return append(items, v)
}

func cloneHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var _ = json.Marshal
