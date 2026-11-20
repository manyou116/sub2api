package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

type logicalSessionKey struct{}

// WithLogicalSession distinguishes a logical conversation from the gateway's
// always-present replay scope. An explicitly empty value disables the header
// fallback, so requests without a session keep their legacy task identity.
// The identity belongs to this local request only; no extra upstream header is
// needed. Gateway callers must already isolate the scope by account and caller.
func WithLogicalSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, logicalSessionKey{}, strings.TrimSpace(sessionID))
}

func logicalSessionFor(r *http.Request) string {
	if sessionID, ok := r.Context().Value(logicalSessionKey{}).(string); ok {
		return sessionID
	}
	// Embedders may supply a logical session header; the standalone slot handler
	// explicitly disables this fallback to keep client requests isolated.
	return strings.TrimSpace(r.Header.Get("session_id"))
}

type upstreamTaskKey struct{}
type upstreamTaskIdentity struct{ taskID, turnID string }

// WithUpstreamTask supplies the task leased by the gateway and this generation's
// turn identity. Public headers and request payloads cannot select this override.
// IDs are validated by PrepareWithOptions; they never replace local replay scope.
func WithUpstreamTask(ctx context.Context, taskID, turnID string) context.Context {
	return context.WithValue(ctx, upstreamTaskKey{}, upstreamTaskIdentity{taskID, turnID})
}

func upstreamTaskFor(r *http.Request) (upstreamTaskIdentity, bool) {
	identity, ok := r.Context().Value(upstreamTaskKey{}).(upstreamTaskIdentity)
	return identity, ok
}

// These private gateway response headers describe the prepared wire metadata,
// not acceptance or persistence by the remote service. Only hashes are exposed.
func writeUpstreamTaskHashes(headers http.Header, prepared []byte) {
	var body struct {
		Metadata struct {
			TaskID string `json:"task_id"`
			TurnID string `json:"turn_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(prepared, &body) != nil || body.Metadata.TaskID == "" || body.Metadata.TurnID == "" {
		return
	}
	for header, value := range map[string]string{
		"X-BPS-Task-ID-Hash": body.Metadata.TaskID,
		"X-BPS-Turn-ID-Hash": body.Metadata.TurnID,
	} {
		sum := sha256.Sum256([]byte(value))
		headers.Set(header, hex.EncodeToString(sum[:]))
	}
}
