package bpsbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const deviceURL = issuer + "/api/accounts/deviceauth/authorize?client_id=" + ClientID

var ErrUnavailable = errors.New("BPS bridge is unavailable")

// Process is the boundary to a separately installed bps-local executable.
type Process struct {
	Binary string
	Dir    string
	Proxy  string
}

type Prompt struct {
	URL  string `json:"url"`
	Code string `json:"code"`
}

func (p Process) command(ctx context.Context, args ...string) *exec.Cmd {
	// Binary is an operator-configured absolute path. Arguments are not shell
	// input and no application environment or credential is inherited.
	//nolint:gosec // The absolute executable path is configured by the operator, not supplied by an API caller.
	cmd := exec.CommandContext(ctx, p.Binary, args...)
	cmd.Env = []string{"HOME=" + p.Dir, "TMPDIR=" + p.Dir, "BPS_LOCAL_PROXY=" + p.Proxy}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func (p Process) Device(ctx context.Context, show func(Prompt)) ([]byte, error) {
	path := filepath.Join(p.Dir, "device.json")
	cmd := p.command(ctx, "--login", "--no-browser", "--auth", path)
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return nil, ErrUnavailable
	}
	reader := bufio.NewScanner(stdout)
	var prompt Prompt
	for reader.Scan() {
		line := reader.Text()
		switch {
		case line == "Open: "+deviceURL:
			prompt.URL = deviceURL
		case strings.HasPrefix(line, "Device code: ") && prompt.URL != "":
			code := strings.TrimPrefix(line, "Device code: ")
			if len(code) >= 4 && len(code) <= 32 && strings.IndexFunc(code, func(r rune) bool {
				return r != '-' && (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z')
			}) == -1 {
				prompt.Code = code
				show(prompt)
			}
		}
	}
	_ = stdout.Close()
	if cmd.Wait() != nil || prompt.Code == "" || reader.Err() != nil {
		return nil, ErrUnavailable
	}
	return ReadGrantFile(path)
}

type BoundGrant struct {
	Grant
	Identity  Identity  `json:"identity"`
	ExpiresAt time.Time `json:"expires_at"`
}

func ReadGrantFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrInvalidGrant
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, MaxGrantBytes+1))
	if err != nil || len(raw) > MaxGrantBytes {
		return nil, ErrInvalidGrant
	}
	return raw, nil
}

func Save(path string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".bps-")
	if err != nil {
		return ErrUnavailable
	}
	defer func() { _ = os.Remove(f.Name()) }()
	err = json.NewEncoder(f).Encode(value)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil || os.Rename(f.Name(), path) != nil {
		return ErrUnavailable
	}
	return nil
}

type Worker struct {
	base string
	stop context.CancelFunc
	Done <-chan struct{}
}

func (w *Worker) Stop() {
	w.stop()
	<-w.Done
}

var localHTTP = &http.Client{
	Transport:     &http.Transport{ResponseHeaderTimeout: 90 * time.Second},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Start only trusts the loopback listener reported by the configured child.
// Port zero lets the OS bind atomically without a reserve/release race.
func (p Process) Start(ctx context.Context, authPath, stateDir string, concurrency int) (*Worker, error) {
	runtime, stop := context.WithCancel(context.Background())
	cmd := p.command(runtime, "--addr", "127.0.0.1:0", "--auth", authPath, "--state-dir", stateDir,
		"--max-concurrent", strconv.Itoa(concurrency), "--max-pending", "0")
	stderr, err := cmd.StderrPipe()
	if err != nil || cmd.Start() != nil {
		stop()
		return nil, ErrUnavailable
	}
	ready, done := make(chan string, 1), make(chan struct{})
	go func() {
		defer close(done)
		reader := bufio.NewScanner(stderr)
		for reader.Scan() {
			_, addr, ok := strings.Cut(reader.Text(), "bridge listening on http://")
			host, port, splitErr := net.SplitHostPort(addr)
			if n, convertErr := strconv.Atoi(port); ok && splitErr == nil && convertErr == nil && n > 0 && n < 65536 && host == "127.0.0.1" {
				select {
				case ready <- addr:
				default:
				}
			}
		}
		_ = stderr.Close()
		_ = cmd.Wait()
	}()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case addr := <-ready:
		return &Worker{base: (&url.URL{Scheme: "http", Host: addr}).String(), stop: stop, Done: done}, nil
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
	stop()
	<-done
	return nil, ErrUnavailable
}

func (w *Worker) Responses(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.base+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := localHTTP.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	return resp, nil
}
