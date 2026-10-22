package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

type openAISlowFirstOutputMetadata struct {
	threshold time.Duration
}
type openAISlowFirstOutputContextKey struct{}

type openAISlowFirstOutputObservation struct {
	elapsed, threshold time.Duration
}

// Keep latency measurement independent of billing TTFT and replay safety.
// Metadata is attached after the final outbound body has been normalized.
func withOpenAISlowFirstOutputMetadata(req *http.Request, account *Account, body []byte) *http.Request {
	if account == nil || account.capacityAttempt == nil || !account.capacityAttempt.settings.SlowFirstOutputEnabled ||
		!gjson.GetBytes(body, "stream").Bool() || IsImageGenerationIntent(req.URL.Path, account.capacityAttempt.model, body) {
		return req
	}
	v := account.capacityAttempt.settings
	seconds := v.SlowFirstOutputSeconds
	if effort := extractOpenAIReasoningEffortFromBody(body, account.capacityAttempt.model); effort != nil {
		switch strings.ToLower(*effort) {
		case "high", "xhigh", "max":
			seconds = v.SlowFirstOutputHighEffortSeconds
		}
	}
	return req.WithContext(context.WithValue(req.Context(), openAISlowFirstOutputContextKey{},
		openAISlowFirstOutputMetadata{time.Duration(seconds) * time.Second}))
}

type openAISlowFirstOutput struct {
	mu        sync.Mutex
	account   *Account
	ctx       context.Context
	start     time.Time
	threshold time.Duration
	timer     *time.Timer
	stop      func() bool
	done      bool
	useful    bool
}

func startOpenAISlowFirstOutput(req *http.Request, account *Account) *openAISlowFirstOutput {
	metadata, ok := req.Context().Value(openAISlowFirstOutputContextKey{}).(openAISlowFirstOutputMetadata)
	if !ok || metadata.threshold <= 0 || account == nil || account.capacityAttempt == nil {
		return nil
	}
	a := account.capacityAttempt
	ctx := a.ctx
	if ctx == nil {
		ctx = req.Context()
	}
	o := &openAISlowFirstOutput{account: account, ctx: ctx, start: time.Now(), threshold: metadata.threshold}
	a.slowMu.Lock()
	a.slowOutput = o
	a.fastFirstOutput = false
	a.slowMu.Unlock()
	// Initialize both callbacks under the lock: cancellation may already be ready.
	o.mu.Lock()
	o.timer = time.AfterFunc(o.threshold, o.expired)
	o.stop = context.AfterFunc(ctx, func() { o.finish(false) })
	o.mu.Unlock()
	return o
}

func (o *openAISlowFirstOutput) expired() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.done && o.ctx.Err() == nil {
		o.recordSlow()
	}
}

// Called with o.mu held; never block the stream reader on Redis or PostgreSQL.
func (o *openAISlowFirstOutput) recordSlow() {
	a := o.account.capacityAttempt
	a.slowMu.Lock()
	first := !a.slowObserved
	a.slowObserved = true
	if first {
		a.slowRecordDone = make(chan struct{})
	}
	done := a.slowRecordDone
	a.slowMu.Unlock()
	if first {
		observation := &openAISlowFirstOutputObservation{time.Since(o.start), o.threshold}
		go func() {
			defer close(done)
			recordOpenAITextCooldown(o.account, 3, observation)
		}()
	}
}

func (o *openAISlowFirstOutput) finish(useful bool) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return
	}
	o.done, o.useful = true, useful
	o.timer.Stop()
	o.stop()
	if o.ctx.Err() != nil {
		return
	}
	if time.Since(o.start) >= o.threshold {
		o.recordSlow()
	} else if useful {
		a := o.account.capacityAttempt
		a.slowMu.Lock()
		a.fastFirstOutput = true
		a.slowMu.Unlock()
	}
}

func openAISlowFirstOutputHealthy(account *Account) bool {
	if account == nil || account.capacityAttempt == nil || !account.capacityAttempt.settings.SlowFirstOutputEnabled {
		return true
	}
	a := account.capacityAttempt
	a.slowMu.Lock()
	defer a.slowMu.Unlock()
	return a.fastFirstOutput && !a.slowObserved
}

// Let the latency rule own pre-output timeouts, preserving its text-only pause.
// A later inter-token timeout still uses the existing stream-timeout policy.
func openAISlowFirstOutputOwnsTimeout(account *Account) bool {
	if account == nil || account.capacityAttempt == nil {
		return false
	}
	a := account.capacityAttempt
	a.slowMu.Lock()
	o := a.slowOutput
	a.slowMu.Unlock()
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.useful || o.ctx.Err() != nil || time.Since(o.start) < o.threshold {
		return false
	}
	o.recordSlow()
	return true
}

// Observe the existing reader without buffering delivery, changing SSE bytes,
// or starting a second read loop. Parsing stops at the first useful content.
type openAISlowFirstOutputBody struct {
	io.ReadCloser
	observer   *openAISlowFirstOutput
	line, data []byte
	eventType  string
	finished   bool
}

func (o *openAISlowFirstOutput) wrapResponse(resp *http.Response, err error) {
	if o == nil {
		return
	}
	if err != nil || resp == nil || resp.StatusCode >= 400 || resp.Body == nil ||
		!strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		o.finish(false)
		return
	}
	resp.Body = &openAISlowFirstOutputBody{ReadCloser: resp.Body, observer: o}
}

func (b *openAISlowFirstOutputBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	for remaining := p[:n]; len(remaining) > 0 && !b.finished; {
		i := bytes.IndexByte(remaining, '\n')
		part := remaining
		if i >= 0 {
			part = remaining[:i]
		}
		// An unusual oversized preamble is inconclusive, not an account fault.
		if len(b.line)+len(part)+len(b.data) > 64*1024 {
			b.finish(false)
			break
		}
		b.line = append(b.line, part...)
		if i < 0 {
			break
		}
		b.consumeLine(strings.TrimSuffix(string(b.line), "\r"))
		b.line = b.line[:0]
		remaining = remaining[i+1:]
	}
	if err != nil {
		if len(b.line) > 0 && !b.finished {
			b.consumeLine(strings.TrimSuffix(string(b.line), "\r"))
		}
		b.finish(false)
	}
	return n, err
}

func (b *openAISlowFirstOutputBody) consumeLine(line string) {
	if line == "" {
		b.eventType, b.data = "", b.data[:0]
		return
	}
	if strings.HasPrefix(line, "event:") {
		b.eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	}
	if data, ok := extractOpenAISSEDataLine(line); ok {
		b.data = append(b.data, data...)
		b.data = append(b.data, '\n')
		payload := string(b.data)
		eventType := effectiveOpenAISSEEventType(b.data, b.eventType)
		meaningful := openAIStreamDataStartsVisibleOutput(payload, eventType)
		if strings.HasSuffix(eventType, ".delta") && gjson.Get(payload, "delta").Type != gjson.String {
			meaningful = false
		}
		if meaningful {
			b.finish(true)
		} else if strings.TrimSpace(payload) == "[DONE]" || openAIStreamEventTypeIsTerminal(eventType) {
			b.finish(false)
		}
	}
}

func (b *openAISlowFirstOutputBody) finish(useful bool) {
	if b.finished {
		return
	}
	b.finished = true
	b.line, b.data = nil, nil
	b.observer.finish(useful)
}

func (b *openAISlowFirstOutputBody) Close() error {
	// Close may race with Read; only the observer is shared with cancellation.
	b.observer.finish(false)
	return b.ReadCloser.Close()
}
