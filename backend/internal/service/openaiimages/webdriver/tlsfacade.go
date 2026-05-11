package webdriver

// tlsfacade.go 提供与 imroc/req v3 链式 API 形态接近的 thin wrapper，底层使用
// bogdanfinn/tls-client。目的：为 webdriver 包内的请求构造提供"换底不换皮"的迁移
// 路径，避免在所有调用点机械替换 SetContext/SetHeaders/SetBodyJsonMarshal/Get/Post
// 等链式调用。
//
// 为什么换：imroc/req + utls 只伪造 TLS ClientHello 字节序列，HTTP/2 SETTINGS、
// HEADERS 顺序、HPACK 等仍是 Go net/http 默认实现；Cloudflare 使用 JA4_H + Akamai
// HTTP/2 fingerprint 可识别为 Go 客户端，导致 chatgpt.com 上稳定 403 challenge。
// bogdanfinn/tls-client 基于真实 BoringSSL（curl-impersonate 同源）+ 完整 HTTP/2
// 帧伪装，实测 CF 通过率显著提升。
//
// 仅暴露 webdriver 包当前实际使用的方法子集；未实现的方法故意保持不存在以避免
// 误用。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// HTTPClient 包装 tls-client.HttpClient 并暴露与 imroc/req 形似的链式 API。
type HTTPClient struct {
	inner   tls_client.HttpClient
	profile profiles.ClientProfile

	mu      sync.Mutex
	headers map[string]string // 默认头（每个请求合并），保持与 req.Client.Headers 一致语义
}

// R 创建一个新的请求构造器。
func (c *HTTPClient) R() *HTTPRequest {
	return &HTTPRequest{client: c, headers: make(map[string][]string)}
}

// SetProxyURL 切换代理（运行期可改）。
func (c *HTTPClient) SetProxyURL(proxyURL string) error {
	return c.inner.SetProxy(proxyURL)
}

// HTTPRequest 与 imroc/req.HTTPRequest 形态接近的构造器。
type HTTPRequest struct {
	client *HTTPClient
	ctx    context.Context

	headers     map[string][]string
	bodyReader  io.Reader
	contentType string

	successResult any // SetSuccessResult(&v)：成功时 JSON 反序列化到 v
}

// SetContext 关联 context。
func (r *HTTPRequest) SetContext(ctx context.Context) *HTTPRequest {
	r.ctx = ctx
	return r
}

// SetHeaders 批量设置请求头（覆盖同名，单值）。
func (r *HTTPRequest) SetHeaders(h map[string]string) *HTTPRequest {
	for k, v := range h {
		r.headers[http.CanonicalHeaderKey(k)] = []string{v}
	}
	return r
}

// SetHeader 设置单个请求头（覆盖）。
func (r *HTTPRequest) SetHeader(k, v string) *HTTPRequest {
	r.headers[http.CanonicalHeaderKey(k)] = []string{v}
	return r
}

// SetHeaderMultiValues 设置 http.Header 风格的多值头（覆盖同名）。
func (r *HTTPRequest) SetHeaderMultiValues(h http.Header) *HTTPRequest {
	for k, vs := range h {
		copied := make([]string, len(vs))
		copy(copied, vs)
		r.headers[http.CanonicalHeaderKey(k)] = copied
	}
	return r
}

// SetBodyJsonMarshal 设置请求体为对象的 JSON 序列化。Content-Type 默认 application/json。
func (r *HTTPRequest) SetBodyJsonMarshal(v any) *HTTPRequest {
	buf, err := json.Marshal(v)
	if err != nil {
		// 与 imroc/req 行为一致：保留错误，发送时返回。
		r.bodyReader = errReader{err: fmt.Errorf("tlsfacade: marshal body: %w", err)}
		return r
	}
	r.bodyReader = bytes.NewReader(buf)
	if r.contentType == "" {
		r.contentType = "application/json"
	}
	return r
}

// SetBodyBytes 设置请求体为字节切片。
func (r *HTTPRequest) SetBodyBytes(b []byte) *HTTPRequest {
	r.bodyReader = bytes.NewReader(b)
	return r
}

// SetBodyString 设置请求体为字符串。
func (r *HTTPRequest) SetBodyString(s string) *HTTPRequest {
	r.bodyReader = strings.NewReader(s)
	return r
}

// SetSuccessResult 设置成功时 JSON 自动反序列化的目标。
// 只在 2xx 响应时生效；与 imroc/req 一致。
func (r *HTTPRequest) SetSuccessResult(v any) *HTTPRequest {
	r.successResult = v
	return r
}

// DisableAutoReadResponse 占位：tls-client 默认就不预读 Body，调用方通过 resp.Body
// 流式消费即可，所以这里是 no-op。保留以匹配旧调用形态，方便机械迁移。
func (r *HTTPRequest) DisableAutoReadResponse() *HTTPRequest {
	return r
}

// Get 发送 GET 请求。
func (r *HTTPRequest) Get(rawURL string) (*HTTPResponse, error) { return r.do(http.MethodGet, rawURL) }

// Post 发送 POST 请求。
func (r *HTTPRequest) Post(rawURL string) (*HTTPResponse, error) {
	return r.do(http.MethodPost, rawURL)
}

// Put 发送 PUT 请求。
func (r *HTTPRequest) Put(rawURL string) (*HTTPResponse, error) { return r.do(http.MethodPut, rawURL) }

func (r *HTTPRequest) do(method, rawURL string) (*HTTPResponse, error) {
	if _, err := url.Parse(rawURL); err != nil {
		return nil, fmt.Errorf("tlsfacade: parse url: %w", err)
	}

	// 提前拦截 SetBodyJsonMarshal 时 marshal 失败。
	if er, ok := r.bodyReader.(errReader); ok {
		return nil, er.err
	}

	// 注意：tls-client 内部使用 bogdanfinn/fhttp（net/http 的 fork），不是 stdlib。
	httpReq, err := fhttp.NewRequest(method, rawURL, r.bodyReader)
	if err != nil {
		return nil, fmt.Errorf("tlsfacade: build request: %w", err)
	}
	if r.ctx != nil {
		httpReq = httpReq.WithContext(r.ctx)
	}

	// 合并默认头 + 请求级头；请求级覆盖默认。
	r.client.mu.Lock()
	for k, v := range r.client.headers {
		httpReq.Header.Set(k, v)
	}
	r.client.mu.Unlock()
	for k, vs := range r.headers {
		httpReq.Header[k] = vs
	}
	if r.contentType != "" && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", r.contentType)
	}

	resp, err := r.client.inner.Do(httpReq)
	if err != nil {
		return nil, err
	}

	// 把 fhttp.Header 转回 stdlib http.Header，方便包内调用方一致使用 net/http 类型。
	stdHeader := make(http.Header, len(resp.Header))
	for k, vs := range resp.Header {
		copied := make([]string, len(vs))
		copy(copied, vs)
		stdHeader[k] = copied
	}

	wrapped := &HTTPResponse{
		StatusCode: resp.StatusCode,
		Header:     stdHeader,
		Body:       resp.Body,
	}

	// 仅在 2xx 时自动反序列化到 successResult；同时缓存 body 以便错误路径再读。
	if r.successResult != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, readErr := io.ReadAll(wrapped.Body)
		_ = wrapped.Body.Close()
		if readErr != nil {
			return wrapped, fmt.Errorf("tlsfacade: read body: %w", readErr)
		}
		wrapped.cachedBody = body
		wrapped.bodyConsumed = true
		wrapped.Body = io.NopCloser(bytes.NewReader(body))
		if err := json.Unmarshal(body, r.successResult); err != nil {
			return wrapped, fmt.Errorf("tlsfacade: unmarshal success result: %w", err)
		}
	}

	return wrapped, nil
}

// HTTPResponse 暴露 imroc/req 风格的响应字段与方法。仅含包内实际使用的字段。
type HTTPResponse struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser

	cachedBody   []byte
	bodyConsumed bool
}

// IsSuccessState 与 imroc/req 同名方法等价：状态码 2xx。
func (r *HTTPResponse) IsSuccessState() bool {
	if r == nil {
		return false
	}
	return r.StatusCode >= 200 && r.StatusCode < 300
}

// IsErrorState 与 imroc/req 同名方法等价：状态码 >=400。
func (r *HTTPResponse) IsErrorState() bool {
	if r == nil {
		return false
	}
	return r.StatusCode >= 400
}

// ToBytes 把响应 body 全量读入字节切片。重复调用安全（结果缓存）。
// 调用后 Body 仍然可读（被替换为 in-memory reader）。
func (r *HTTPResponse) ToBytes() ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("tlsfacade: nil response")
	}
	if r.bodyConsumed {
		return r.cachedBody, nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil, err
	}
	r.cachedBody = body
	r.bodyConsumed = true
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// Bytes 与 ToBytes 等价但忽略错误（与 imroc/req 同名方法对齐）。
func (r *HTTPResponse) Bytes() []byte {
	b, _ := r.ToBytes()
	return b
}

// errReader 用作 SetBodyJsonMarshal marshal 失败时的占位 body reader，do() 会拦截。
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// newTLSHTTPClient 构造底层 tls-client 实例。
func newTLSHTTPClient(proxyURL string, fp Fingerprint, timeout time.Duration) (*HTTPClient, error) {
	jar := tls_client.NewCookieJar()
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(int(timeout / time.Second)),
		tls_client.WithClientProfile(fp.Profile),
		tls_client.WithCookieJar(jar),
		// 默认跟随重定向；与 imroc/req 默认行为一致。
	}
	if trimmed := strings.TrimSpace(proxyURL); trimmed != "" {
		opts = append(opts, tls_client.WithProxyUrl(trimmed))
	}
	inner, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("tlsfacade: new client: %w", err)
	}
	return &HTTPClient{inner: inner, profile: fp.Profile, headers: make(map[string]string)}, nil
}
