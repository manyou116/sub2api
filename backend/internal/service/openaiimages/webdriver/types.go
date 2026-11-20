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
	AccountID   string
	SessionID   string
	// ClientProfile is opt-in; empty uses the ordinary web context.
	ClientProfile string
}

type InputImage struct {
	FileName    string
	ContentType string
	Data        []byte
}

type GenerateRequest struct {
	Prompt, Model, Size, Quality, ResponseFormat string
	// Mode is chosen by the account type, never inferred from a model suffix.
	Mode string
	// Nil uses the mode default: required for Work, optional for ordinary Web.
	RequireDownloadAttachment *bool
	// ThinkingEffort applies to explicit models; auto delegates reasoning to upstream.
	ThinkingEffort string
	N              int
	Images         []InputImage
	Mask           *InputImage
}

type ImageData struct {
	B64JSON, URL, RevisedPrompt string
	Width, Height               int
	Format                      string
}

type Usage struct{ InputTokens, OutputTokens, ImageOutputTokens int }
type Meta struct {
	ConversationID, Stage string
	Duration              time.Duration
	DownloadAttachment    bool
}
type GenerateResult struct {
	Created int64
	Data    []ImageData
	Usage   Usage
	Meta    Meta
}
type Quota struct {
	Known     bool
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

// SubmissionState records whether retrying generation can duplicate upstream work.
type SubmissionState string

const (
	SubmissionNotSent   SubmissionState = ""
	SubmissionUncertain SubmissionState = "uncertain"
	SubmissionAccepted  SubmissionState = "accepted"
)

type Error struct {
	Kind           ErrorKind
	Code           string
	Message        string
	StatusCode     int
	Retryable      bool
	Stage          string
	ResetAt        *time.Time
	RetryAfter     time.Duration
	Submission     SubmissionState
	ConversationID string
	Cause          error
	// ResponseBody preserves the upstream error payload for callers that need
	// to apply account-state policy without treating all HTTP 401s alike.
	ResponseBody []byte
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

func (e *Error) Unwrap() error          { return e.Cause }
func (e *Error) MayHaveGenerated() bool { return e != nil && e.Submission != SubmissionNotSent }

func NewError(kind ErrorKind, stage, message string, status int, retryable bool) *Error {
	return &Error{Kind: kind, Stage: stage, Message: message, StatusCode: status, Retryable: retryable}
}
