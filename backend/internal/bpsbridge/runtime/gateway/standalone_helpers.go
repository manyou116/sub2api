package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// fixed preserves the credential snapshot selected before a request queues.
type fixed struct{ token, account string }

func (f fixed) Credentials() (string, string, error) {
	if f.token == "" {
		return "", "", errors.New("request credentials required")
	}
	return f.token, f.account, nil
}

func (f fixed) Expiry() time.Time { return time.Time{} }

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
}
