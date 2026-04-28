// Package webdriver implements ChatGPT Web (chatgpt.com /backend-api/f/conversation)
// reverse-proxy 图片生成 / 编辑链路。所有协议事实（PoW 常量表、sentinel 路由、
// SSE 帧解析、三步上传协议）均封装在此子包内，对外只暴露 Driver.Forward 单方法。
//
// 该子包**不依赖**父包 openaiimages 任何类型，从而避免循环 import；上层包通过
// AccountInfo / Upload / Result 这些 plain struct 完成数据传递。
package webdriver

import "time"

// AccountInfo 是上游 ChatGPT Web API 所需的所有账号凭据快照。
// 由调用方（service/openaiimages 层）从 Account 实体投影后传入。
type AccountInfo struct {
	AccountID        int64
	AccessToken      string
	ChatGPTAccountID string
	UserAgent        string
	DeviceID         string
	SessionID        string
	ProxyURL         string
}

// Upload 描述待上传到 ChatGPT 的源图。
type Upload struct {
	Filename    string
	ContentType string
	Width       int
	Height      int
	Data        []byte
}

// Request 描述一次完整的 web 反代生图调用。
type Request struct {
	Account         AccountInfo
	Model           string  // 上游 web 模型 slug 由 Driver 内部映射
	Prompt          string
	N               int
	Uploads         []Upload
	AllowEarlyExit  bool   // 仅纯生图（无 Uploads）开启 SSE 早退
	ResponseFormat  string // 仅供日志，不影响行为
}

// Image 是 webdriver 返回的单张图片二进制。
type Image struct {
	Bytes       []byte
	ContentType string
	Pointer     string // 上游 file-service:// 或 sediment:// pointer，用于排查
}

// Result 是 Forward 的成功结果。
type Result struct {
	ConversationID string
	Images         []Image
	FirstTokenMs   *int
	Duration       time.Duration
	Usage          map[string]any // 透传上游 usage（图片调用通常无意义，保留以备后用）
	RequestID      string
}

// pointerInfo 是 SSE / 轮询返回的图片 pointer。
type pointerInfo struct {
	Pointer string
}

// uploadedFile 是 ChatGPT files 接口返回的内部记录。
type uploadedFile struct {
	FileID      string
	FileName    string
	FileSize    int
	ContentType string
	Width       int
	Height      int
}

// chatRequirements 镜像 sentinel/chat-requirements 响应。
type chatRequirements struct {
	Token     string `json:"token"`
	Turnstile struct {
		Required bool `json:"required"`
	} `json:"turnstile"`
	Arkose struct {
		Required bool `json:"required"`
	} `json:"arkose"`
	ProofOfWork struct {
		Required   bool   `json:"required"`
		Seed       string `json:"seed"`
		Difficulty string `json:"difficulty"`
	} `json:"proofofwork"`
}
