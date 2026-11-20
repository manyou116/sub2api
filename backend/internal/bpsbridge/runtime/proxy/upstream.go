package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/auth"
	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/retryafter"
)

// Real JWT claims take precedence over an externally supplied account field.
// Legacy standalone sources without JWT claims retain their explicit account.
func admissionKeyForToken(token, accountID string) string {
	if key := auth.AdmissionKeyFromToken(token); key != "" {
		return key
	}
	return auth.AdmissionKey(accountID, "")
}

type upstreamRejection struct {
	status int
	retry  int
}

func (e *upstreamRejection) Error() string {
	// Upstream bodies/headers can echo credentials; report only the status.
	return fmt.Sprintf("upstream returned HTTP %d", e.status)
}

func (s *Server) openUpstream(ctx context.Context, body []byte, token, accountID string) (io.ReadCloser, error) {
	if s.cfg.ARCTextSession {
		return s.openARCUpstream(ctx, body, token, accountID)
	}
	if s.cfg.ToolsVersion != "" {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("invalid prepared request")
		}
		metadata := make(map[string]json.RawMessage)
		if err := json.Unmarshal(doc["metadata"], &metadata); err != nil || metadata == nil {
			return nil, fmt.Errorf("invalid prepared metadata")
		}
		metadata["bps_tools_version_id"], _ = json.Marshal(s.cfg.ToolsVersion)
		doc["metadata"], _ = json.Marshal(metadata)
		body, _ = json.Marshal(doc)
	}
	if s.cfg.UpstreamTransport == "ws" {
		return s.openWebSocket(ctx, body, token, accountID)
	}
	endpoint := strings.Replace(strings.Replace(s.cfg.Upstream, "wss://", "https://", 1), "ws://", "http://", 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("could not construct upstream request")
	}
	applyUpstreamHeaders(req, token, accountID)
	s.applyCustomHeaders(req)
	var resp *http.Response
	err = s.startRates.runDispatch(ctx, admissionKeyForToken(token, accountID), s.startAuthorization, s.startDeadline, func(release func()) error {
		// Release the dispatch gate after writing the request, not after waiting
		// for response headers. The rate limit must not serialize whole streams.
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			WroteRequest: func(httptrace.WroteRequestInfo) { release() },
		}))
		var sendErr error
		//nolint:gosec // The endpoint is fixed runtime configuration. Request JSON cannot change it and redirects are rejected.
		resp, sendErr = s.client.Do(req)
		return sendErr
	})
	if err != nil {
		var pacing *StartRateError
		if errors.As(err, &pacing) || ctx.Err() != nil {
			return nil, err
		}
		return nil, fmt.Errorf("could not reach upstream SSE endpoint")
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, &upstreamRejection{status: resp.StatusCode, retry: retryafter.Seconds(resp.Header.Get("Retry-After"))}
	}
	return resp.Body, nil
}
