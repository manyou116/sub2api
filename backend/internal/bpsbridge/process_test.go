package bpsbridge

import (
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBPSDeviceProcessReadsPromptWithoutLoggingGrant(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fixture")
	// Device login runs a known executable and reads a file in its private directory.
	script := "#!/bin/sh\nprintf '%s\\n' 'Open: " + deviceURL + "' 'Device code: TEST-1234'\nprintf '%s' '{\"access_token\":\"fixture\"}' > \"$HOME/device.json\"\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0700))
	var prompt Prompt
	grant, err := (Process{Binary: bin, Dir: dir}).Device(t.Context(), func(value Prompt) { prompt = value })
	require.NoError(t, err)
	require.Equal(t, Prompt{URL: deviceURL, Code: "TEST-1234"}, prompt)
	require.JSONEq(t, `{"access_token":"fixture"}`, string(grant))
}

func TestBPSBridgeBinaryLoopbackSmoke(t *testing.T) {
	bin := os.Getenv("BPS_SMOKE_BINARY")
	if bin == "" {
		t.Skip("a local bps-local binary was not supplied")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "grant.json")
	// Only /healthz is called. This placeholder is intentionally unsigned.
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"fixture-workspace"}}`))
	bound := BoundGrant{Grant: Grant{AccessToken: "e30." + claims + ".fixture", TokenType: "Bearer", ClientID: ClientID}, ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, Save(file, bound))
	worker, err := (Process{Binary: bin, Dir: dir}).Start(t.Context(), file, filepath.Join(dir, "state"), 1)
	require.NoError(t, err)
	defer worker.Stop()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, worker.base+"/healthz", nil)
	require.NoError(t, err)
	response, err := localHTTP.Do(req)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
}
