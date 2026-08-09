package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHealthReflectsShuttingDown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Ensure clean state for this package-level flag.
	shuttingDown.Store(false)
	t.Cleanup(func() { shuttingDown.Store(false) })

	r := gin.New()
	RegisterCommonRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("before shutdown: status = %d, want %d", w.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("before shutdown: status body = %v, want ok", body["status"])
	}

	MarkShuttingDown()

	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("after shutdown: status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	body = map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "draining" {
		t.Fatalf("after shutdown: status body = %v, want draining", body["status"])
	}
}
