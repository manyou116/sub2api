package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// This implements the captured control lifecycle, not an Excel tool executor.
// No captured session IDs, cookies or control URLs are persisted or reused.
func arcTextOnly(raw []byte) bool {
	var d struct {
		Tools []json.RawMessage `json:"tools"`
		Input json.RawMessage   `json:"input"`
	}
	if json.Unmarshal(raw, &d) != nil || len(d.Tools) > 0 {
		return false
	}
	var text string
	if json.Unmarshal(d.Input, &text) == nil {
		return true
	}
	var items []struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if json.Unmarshal(d.Input, &items) != nil {
		return false
	}
	for _, x := range items {
		if x.Type != "" && x.Type != "message" {
			return false
		}
		if x.Role != "user" && x.Role != "assistant" && x.Role != "developer" && x.Role != "system" {
			return false
		}
	}
	return true
}

type arcSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	conn   *websocket.Conn
	wg     sync.WaitGroup
	once   sync.Once
}

func (a *arcSession) Close() {
	a.once.Do(func() {
		a.cancel()
		if a.conn != nil {
			_ = a.conn.CloseNow()
		}
		a.wg.Wait()
	})
}

type arcBody struct {
	io.ReadCloser
	session *arcSession
}

func (b *arcBody) Close() error { err := b.ReadCloser.Close(); b.session.Close(); return err }

func (s *Server) openARCUpstream(ctx context.Context, body []byte, token, account string) (io.ReadCloser, error) {
	// Isolate newly issued cookies and connections to this request/account.
	clone := *s
	clone.cfg.ARCTextSession = false
	client := *s.client
	jar, _ := cookiejar.New(nil)
	client.Jar = jar
	clone.client = &client
	session, err := clone.startARC(ctx, body, token, account)
	if err != nil {
		return nil, err
	}
	upstream, err := clone.openUpstream(session.ctx, body, token, account)
	if err != nil {
		session.Close()
		return nil, err
	}
	return &arcBody{ReadCloser: upstream, session: session}, nil
}

func (s *Server) arcHTTP(ctx context.Context, method, path string, payload any, token, account string, result any) error {
	u, err := url.Parse(s.cfg.Upstream)
	if err != nil {
		return fmt.Errorf("ARC invalid upstream")
	}
	if u.Scheme == "wss" {
		u.Scheme = "https"
	}
	if u.Scheme == "ws" {
		u.Scheme = "http"
	}
	u.Path = "/basispoints/api/" + path
	u.RawQuery = ""
	u.Fragment = ""
	var body io.Reader
	if payload != nil {
		b, e := json.Marshal(payload)
		if e != nil {
			return fmt.Errorf("ARC invalid payload")
		}
		body = bytes.NewReader(b)
	}
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(c, method, u.String(), body)
	applyUpstreamHeaders(req, token, account)
	s.applyCustomHeaders(req)
	req.Header.Set("Accept", "application/json")
	for k, v := range excelClientInfo() {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("ARC %s transport failed", arcStage(path))
	}
	defer func() { _ = resp.Body.Close() }()
	if s.cfg.Verbose {
		log.Printf("ARC stage=%s status=%d", arcStage(path), resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ARC %s returned HTTP %d", arcStage(path), resp.StatusCode)
	}
	if result == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return err
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result) != nil {
		return fmt.Errorf("ARC %s invalid JSON", arcStage(path))
	}
	return nil
}
func arcStage(path string) string {
	if strings.HasSuffix(path, "/ready") {
		return "ready"
	}
	if strings.HasSuffix(path, "/heartbeat") {
		return "heartbeat"
	}
	switch path {
	case "responses/access", "accounts/check", "arc/settings", "arc/executors/register", "task-title":
		return path
	}
	return "unknown"
}

// Control URLs contain a server-issued verify value. Never log them and never
// send the account bearer to this separate host. Restrict the destination.
func validARCControlURL(raw, upstream string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "wss" && u.Host == "ws.chatgpt.com" && strings.HasPrefix(u.Path, "/p5/ws/user/") {
		return true
	}
	base, e := url.Parse(upstream)
	return e == nil && IsLoopback(base.Host) && u.Host == base.Host && u.Scheme == "ws"
}
func validARCExecutorID(id string) bool {
	if !strings.HasPrefix(id, "bp_arc_e_") || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}
func (s *Server) startARC(parent context.Context, body []byte, token, account string) (a *arcSession, err error) {
	ctx, cancel := context.WithCancel(parent)
	a = &arcSession{ctx: ctx, cancel: cancel}
	defer func() {
		if err != nil {
			a.Close()
		}
	}()
	var access struct {
		Allowed bool `json:"allowed"`
	}
	if err = s.arcHTTP(ctx, "GET", "responses/access", nil, token, account, &access); err != nil {
		return
	}
	if !access.Allowed {
		err = fmt.Errorf("ARC access not allowed")
		return
	}
	var accounts map[string]json.RawMessage
	if err = s.arcHTTP(ctx, "GET", "accounts/check", nil, token, account, &accounts); err != nil {
		return
	}
	var settings struct {
		OptedOut bool `json:"opted_out"`
	}
	if err = s.arcHTTP(ctx, "GET", "arc/settings", nil, token, account, &settings); err != nil {
		return
	}
	if settings.OptedOut {
		err = fmt.Errorf("ARC is opted out; no settings were changed")
		return
	}
	var registration struct {
		OK   bool   `json:"ok"`
		ID   string `json:"executor_session_id"`
		Sync struct {
			URL   string `json:"websocket_url"`
			Topic string `json:"command_topic_id"`
		} `json:"client_sync"`
	}
	payload := map[string]any{"surface": "excel", "document_id": newRequestID(), "document_title": "bps-local text-only diagnostic (no workbook)", "supported_tools": []any{}}
	if err = s.arcHTTP(ctx, "POST", "arc/executors/register", payload, token, account, &registration); err != nil {
		return
	}
	if !registration.OK || !validARCExecutorID(registration.ID) || registration.Sync.Topic == "" || !validARCControlURL(registration.Sync.URL, s.cfg.Upstream) {
		err = fmt.Errorf("ARC registration returned invalid control configuration")
		return
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 20*time.Second)
	header := http.Header{"Origin": []string{"https://bps.openai.com"}}
	var resp *http.Response
	a.conn, resp, err = websocket.Dial(dialCtx, registration.Sync.URL, &websocket.DialOptions{HTTPClient: s.client, HTTPHeader: header})
	dialCancel()
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		err = fmt.Errorf("ARC control handshake failed")
		return
	}
	a.conn.SetReadLimit(1 << 20)
	setupCtx, setupCancel := context.WithTimeout(ctx, 20*time.Second)
	defer setupCancel()
	commands := []any{map[string]any{"id": 1, "command": map[string]any{"type": "connect"}}, map[string]any{"id": 2, "command": map[string]any{"type": "subscribe", "topic_id": registration.Sync.Topic}}}
	data, _ := json.Marshal(commands)
	if a.conn.Write(setupCtx, websocket.MessageText, data) != nil {
		err = fmt.Errorf("ARC control setup write failed")
		return
	}
	connected, subscribed := false, false
	for !connected || !subscribed {
		kind, b, e := a.conn.Read(setupCtx)
		if e != nil || kind != websocket.MessageText {
			err = fmt.Errorf("ARC control setup read failed")
			return
		}
		var messages []struct {
			ID    int             `json:"id"`
			Type  string          `json:"type"`
			Error json.RawMessage `json:"error"`
			Reply struct {
				Type  string `json:"type"`
				Topic string `json:"topic_id"`
			} `json:"reply"`
		}
		if json.Unmarshal(b, &messages) != nil {
			err = fmt.Errorf("ARC invalid control reply")
			return
		}
		for _, m := range messages {
			if len(m.Error) > 0 && string(m.Error) != "null" {
				err = fmt.Errorf("ARC control setup rejected")
				return
			}
			if m.Type != "reply" {
				err = fmt.Errorf("ARC unexpected control message during setup")
				return
			}
			if m.ID == 1 && m.Reply.Type == "connect" {
				connected = true
			} else if m.ID == 2 && m.Reply.Type == "subscribe" && m.Reply.Topic == registration.Sync.Topic {
				subscribed = true
			} else {
				err = fmt.Errorf("ARC mismatched control acknowledgement")
				return
			}
		}
	}
	if s.cfg.Verbose {
		log.Printf("ARC control connected and subscribed")
	}
	var ready struct {
		OK bool   `json:"ok"`
		ID string `json:"executor_session_id"`
	}
	path := "arc/executors/" + registration.ID
	if err = s.arcHTTP(ctx, "POST", path+"/ready", nil, token, account, &ready); err != nil {
		return
	}
	if !ready.OK || ready.ID != registration.ID {
		err = fmt.Errorf("ARC executor did not become ready")
		return
	}
	// No tools were registered. Unexpected application messages fail closed;
	// never acknowledge a workbook operation that this process cannot execute.
	a.wg.Add(2)
	go func() {
		defer a.wg.Done()
		for {
			_, data, e := a.conn.Read(ctx)
			if e != nil {
				cancel()
				return
			}
			if strings.TrimSpace(string(data)) != "[]" {
				if s.cfg.Verbose {
					log.Printf("ARC unsupported control message; ending text session")
				}
				cancel()
				return
			}
		}
	}()
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var beat struct {
					OK bool   `json:"ok"`
					ID string `json:"executor_session_id"`
				}
				if s.arcHTTP(ctx, "POST", path+"/heartbeat", nil, token, account, &beat) != nil || !beat.OK || beat.ID != registration.ID {
					cancel()
					return
				}
			}
		}
	}()
	// Task-title shares the exact task ID used by the prepared generation request.
	var doc struct {
		Metadata struct {
			Task string `json:"task_id"`
		} `json:"metadata"`
		Input []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &doc) != nil {
		err = fmt.Errorf("ARC invalid prepared generation request")
		return
	}
	userText := ""
	for _, m := range doc.Input {
		if m.Role == "user" {
			parts := []string{}
			for _, c := range m.Content {
				parts = append(parts, c.Text)
			}
			userText = strings.Join(parts, "\n")
		}
	}
	var title map[string]json.RawMessage
	if err = s.arcHTTP(ctx, "POST", "task-title", map[string]any{"task_id": doc.Metadata.Task, "locale": "zh-CN", "user_message": userText}, token, account, &title); err != nil {
		return
	}
	return a, nil
}
