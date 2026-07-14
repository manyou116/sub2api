package webdriver

import (
	"fmt"
	"time"
)

type Auth struct {
	AccessToken string
	ProxyURL    string
	UserAgent   string
	DeviceID    string
}

type InputImage struct {
	FileName    string
	ContentType string
	Data        []byte
}

type GenerateRequest struct {
	Prompt, Model, Size, Quality, ResponseFormat string
	// ThinkingEffort is ChatGPT web thinking_effort (extended/high/medium/...). Empty => driver default.
	ThinkingEffort string
	N              int
	Images         []InputImage
	Mask           *InputImage
}

type ImageData struct {
	B64JSON, URL, RevisedPrompt string
}

type Usage struct{ InputTokens, OutputTokens, ImageOutputTokens int }
type Meta struct {
	ConversationID, Stage string
	Duration              time.Duration
}
type GenerateResult struct {
	Created int64
	Data    []ImageData
	Usage   Usage
	Meta    Meta
}
type Quota struct {
	Remaining int
	ResetAt   *time.Time
	ProbedAt  time.Time
	Raw       string
}

type ErrorKind string

const (
	ErrorKindAuth        ErrorKind = "auth"
	ErrorKindRateLimited ErrorKind = "rate_limited"
	ErrorKindPolicy      ErrorKind = "policy"
	ErrorKindTransport   ErrorKind = "transport"
	ErrorKindTimeout     ErrorKind = "timeout"
	ErrorKindUpstream    ErrorKind = "upstream"
	ErrorKindInternal    ErrorKind = "internal"
)

type Error struct {
	Kind       ErrorKind
	Message    string
	StatusCode int
	Retryable  bool
	Stage      string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Stage != "" {
		return fmt.Sprintf("webdriver %s: %s", e.Stage, e.Message)
	}
	return e.Message
}

func NewError(kind ErrorKind, stage, message string, status int, retryable bool) *Error {
	return &Error{Kind: kind, Stage: stage, Message: message, StatusCode: status, Retryable: retryable}
}
