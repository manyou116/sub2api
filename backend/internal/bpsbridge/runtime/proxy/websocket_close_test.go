package proxy

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func closeTestBody(t *testing.T, up *httptest.Server, ctx context.Context, customTransport bool) io.ReadCloser {
	t.Helper()
	s := wsBridge(t, up, nil)
	if customTransport {
		// Exercise the path where scopedWebSocketClient cannot clone a concrete
		// *http.Transport; the upgraded body must still be forcibly closeable.
		s.client.Transport = closeTestRoundTripper{s.client.Transport}
	}
	body, err := s.openWebSocket(ctx, []byte(`{"model":"test","input":"hi"}`), "test-token", "test-account")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = body.Close() })
	return body
}

type closeTestRoundTripper struct{ http.RoundTripper }

func TestWebSocketTerminalUsesNormalClose(t *testing.T) {
	for _, event := range []string{"response.completed", "response.failed", "response.incomplete", "error"} {
		t.Run(event, func(t *testing.T) {
			closed := make(chan websocket.StatusCode, 1)
			var attempts atomic.Int32
			up := wsMock(t, func(ctx context.Context, c *websocket.Conn, _ map[string]any, _ *http.Request) {
				attempts.Add(1)
				sendWS(t, ctx, c, map[string]any{"type": event})
				_, _, err := c.Read(ctx)
				closed <- websocket.CloseStatus(err)
			})
			body := closeTestBody(t, up, context.Background(), false)
			data, err := io.ReadAll(body)
			if err != nil || !strings.Contains(string(data), "event: "+event) {
				t.Fatalf("terminal event lost: %q, %v", data, err)
			}
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case code := <-closed:
				if code != websocket.StatusNormalClosure {
					t.Fatalf("close code = %d, want 1000", code)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("peer did not observe normal close")
			}
			if attempts.Load() != 1 {
				t.Fatalf("attempts = %d, request was replayed", attempts.Load())
			}
		})
	}
}

// This raw loopback peer deliberately reads but does not acknowledge a close
// frame. A websocket.Conn peer would automatically acknowledge from Read.
func unresponsiveClosePeer(t *testing.T, response string) (*httptest.Server, <-chan int, <-chan struct{}, *atomic.Int32) {
	t.Helper()
	observed := make(chan int, 1)
	ended := make(chan struct{})
	attempts := new(atomic.Int32)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) != 1 {
			http.Error(w, "unexpected replay", http.StatusBadRequest)
			return
		}
		defer close(ended)
		conn, rw, err := fixtureValue[http.Hijacker](w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
		hash := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Protocol: responses\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(hash[:]))
		_ = rw.Flush()
		if op, _, err := readCloseTestFrame(rw.Reader); err != nil || op != 1 {
			t.Errorf("response.create frame: opcode=%d err=%v", op, err)
			return
		}
		if len(response) >= 126 {
			t.Error("test response too long")
			return
		}
		_, _ = rw.Write([]byte{0x81, byte(len(response))})
		_, _ = rw.WriteString(response)
		_ = rw.Flush()
		op, data, err := readCloseTestFrame(rw.Reader)
		if err != nil {
			observed <- -1 // Immediate transport close, without a normal close frame.
			return
		}
		if op == 8 && len(data) >= 2 {
			observed <- int(binary.BigEndian.Uint16(data[:2]))
		} else {
			observed <- -2
		}
		// The peer never responds: the client must terminate the underlying
		// transport at its deadline rather than leave a close goroutine behind.
		if _, _, err := readCloseTestFrame(rw.Reader); err == nil {
			t.Error("unexpected second frame while waiting for transport release")
		}
	}))
	t.Cleanup(up.Close)
	return up, observed, ended, attempts
}

func readCloseTestFrame(r *bufio.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	n := uint64(header[1] & 127)
	switch n {
	case 126:
		var size [2]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(size[:]))
	case 127:
		var size [8]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(size[:])
	}
	if n > 1<<20 {
		return 0, nil, fmt.Errorf("test frame too large: %d", n)
	}
	var mask [4]byte
	if header[1]&128 != 0 {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	data := make([]byte, n)
	_, err := io.ReadFull(r, data)
	if header[1]&128 != 0 {
		for i := range data {
			data[i] ^= mask[i%4]
		}
	}
	return header[0] & 15, data, err
}

func TestWebSocketNormalCloseDeadlineAndCancellation(t *testing.T) {
	for _, cancelDuringClose := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelDuringClose), func(t *testing.T) {
			up, observed, ended, attempts := unresponsiveClosePeer(t, `{"type":"response.completed"}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := closeTestBody(t, up, ctx, true)
			if _, err := io.ReadAll(body); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			closed := make(chan struct{})
			go func() { _ = body.Close(); close(closed) }()
			select {
			case code := <-observed:
				if code != 1000 {
					t.Fatalf("close code = %d", code)
				}
			case <-time.After(time.Second):
				t.Fatal("normal close not initiated")
			}
			limit := 2 * webSocketCloseTimeout
			if cancelDuringClose {
				cancel()
				limit = 500 * time.Millisecond
			}
			select {
			case <-closed:
			case <-time.After(limit):
				t.Fatal("close did not respect deadline/cancellation")
			}
			if !cancelDuringClose && time.Since(started) < webSocketCloseTimeout/2 {
				t.Fatal("did not wait for peer handshake before forced release")
			}
			select {
			case <-ended:
			case <-time.After(time.Second):
				t.Fatal("underlying socket still open")
			}
			if attempts.Load() != 1 {
				t.Fatal("request was retried")
			}
		})
	}
}

func TestWebSocketLocalFailureForcesClose(t *testing.T) {
	up, observed, ended, attempts := unresponsiveClosePeer(t, "not JSON")
	body := closeTestBody(t, up, context.Background(), true)
	data, err := io.ReadAll(body)
	if err != nil || !strings.Contains(string(data), "basispoints_ws_protocol_error") {
		t.Fatalf("unexpected stream result %q, %v", data, err)
	}
	started := time.Now()
	_ = body.Close()
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("local failure waited for normal close")
	}
	select {
	case code := <-observed:
		if code != -1 {
			t.Fatalf("local protocol failure sent close frame %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("socket was not released")
	}
	<-ended
	if attempts.Load() != 1 {
		t.Fatal("request was retried")
	}
}

func TestWebSocketCloseConcurrentWithTerminalRead(t *testing.T) {
	// Exercise the terminal-state handoff under -race; Close may be called by
	// downstream cancellation while the single reader is consuming the terminal.
	for range 20 {
		up := wsMock(t, func(ctx context.Context, c *websocket.Conn, _ map[string]any, _ *http.Request) {
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed"}`))
			_, _, _ = c.Read(ctx)
		})
		body := closeTestBody(t, up, context.Background(), false)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = io.Copy(io.Discard, body) }()
		go func() { defer wg.Done(); _ = body.Close() }()
		wg.Wait()
		up.Close()
	}
}
