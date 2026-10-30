package webdriver

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/tidwall/gjson"
)

var stageLogURL = regexp.MustCompile(`(?i)(?:https?|socks5h?)://[^\s"<>]+`)
var stageLogBearer = regexp.MustCompile(`(?i)bearer\s+[^\s",;]+`)
var stageLogJWT = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

func safeStageLogMessage(message string) string {
	message = stageLogURL.ReplaceAllString(message, "[url]")
	message = stageLogBearer.ReplaceAllString(message, "Bearer [redacted]")
	message = stageLogJWT.ReplaceAllString(message, "[token]")
	message = logredact.RedactText(message, "token", "authorization", "cookie", "proxy_authorization")
	return strings.ReplaceAll(strings.ReplaceAll(message, "\n", " "), "\r", " ")
}

func transportError(stage string, cause error) *Error {
	err := NewError(ErrorKindTransport, stage, cause.Error(), 0, true)
	err.Cause = cause
	return err
}

func submissionError(err error, state SubmissionState, conversationID string) *Error {
	var source *Error
	if !errors.As(err, &source) {
		source = transportError("generation", err)
	}
	result := *source
	result.Submission, result.ConversationID = state, conversationID
	return &result
}

func parseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	var delay time.Duration
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > 86400 {
			seconds = 86400
		}
		delay = time.Duration(seconds) * time.Second
	} else if until, err := http.ParseTime(raw); err == nil {
		delay = until.Sub(now)
	}
	if delay < 0 {
		return 0
	}
	if delay > 24*time.Hour {
		return 24 * time.Hour
	}
	return delay
}

func containsAny(text string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(text, candidate) {
			return true
		}
	}
	return false
}

func isCredentialRejection(body string) bool {
	code := strings.ToLower(gjson.Get(body, "error.code").String())
	if code == "" {
		code = strings.ToLower(gjson.Get(body, "error.type").String())
	}
	switch code {
	case "token_invalidated", "invalid_token", "token_expired", "invalid_authentication", "account_deactivated", "session_expired":
		return true
	}
	return false
}

// Closing the response unblocks Scan; cancellation also unblocks a full channel.
func readSSELines(ctx context.Context, body io.Reader) (<-chan string, <-chan error) {
	lines, failures := make(chan string, 64), make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			failures <- err
		}
	}()
	return lines, failures
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Only repeat safe read requests; never replay the conversation POST.
func retryDownload(ctx context.Context, attempts int, step time.Duration, read func() ([]byte, error)) ([]byte, error) {
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		data, err := read()
		if err == nil {
			return data, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, transportError("download", ctx.Err())
		}
		var typed *Error
		if errors.As(err, &typed) && (typed.Kind == ErrorKindAuth || typed.Kind == ErrorKindPolicy) {
			break
		}
		retryable := typed != nil && typed.Retryable
		retryable = retryable || containsAny(strings.ToLower(err.Error()), "not found", "missing download url")
		if !retryable || attempt+1 == attempts {
			break
		}
		delay := time.Duration(attempt+1) * step
		if typed != nil && typed.RetryAfter > delay {
			delay = typed.RetryAfter
		}
		if err := waitForRetry(ctx, delay); err != nil {
			return nil, transportError("download", err)
		}
	}
	return nil, last
}
