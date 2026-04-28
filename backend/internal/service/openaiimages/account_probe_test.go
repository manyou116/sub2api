package openaiimages

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type stubRepo struct {
	mu      sync.Mutex
	updates map[int64]map[string]any
}

func (s *stubRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updates == nil {
		s.updates = map[int64]map[string]any{}
	}
	merged := s.updates[id]
	if merged == nil {
		merged = map[string]any{}
		s.updates[id] = merged
	}
	for k, v := range updates {
		merged[k] = v
	}
	return nil
}

func (s *stubRepo) get(id int64) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{}
	for k, v := range s.updates[id] {
		out[k] = v
	}
	return out
}

func TestAccountProbe_RefreshAccount_HappyPath(t *testing.T) {
	ResetProbeThrottleForTest()
	resetAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		if !checkProbeHeaders(t, r, "/backend-api/me") {
			http.Error(w, "bad headers", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email":             "u@x.com",
			"account_plan_type": "ChatGPT Plus",
		})
	})
	mux.HandleFunc("/init", func(w http.ResponseWriter, r *http.Request) {
		if !checkProbeHeaders(t, r, "/backend-api/conversation/init") {
			http.Error(w, "bad headers", 400)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["timezone_offset_min"] == nil {
			t.Errorf("init body missing timezone_offset_min: %v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"limits_progress": []map[string]any{
				{"feature_name": "voice_chat", "remaining": 10},
				{
					"feature_name": "image_gen",
					"remaining":    7,
					"limit":        40,
					"reset_after":  resetAt.Format(time.RFC3339),
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	repo := &stubRepo{}
	probe := NewAccountProbe(repo)
	probe.MeURL = srv.URL + "/me"
	probe.InitURL = srv.URL + "/init"
	probe.Now = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

	res, err := probe.RefreshAccount(context.Background(), ProbeAccount{
		ID: 42, AccessToken: "tok",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Email != "u@x.com" {
		t.Errorf("email=%q", res.Email)
	}
	if res.AccountPlan != "plus" {
		t.Errorf("plan=%q", res.AccountPlan)
	}
	if res.QuotaRemaining != 7 {
		t.Errorf("remaining=%d", res.QuotaRemaining)
	}
	if res.QuotaTotal != 40 {
		t.Errorf("total=%d", res.QuotaTotal)
	}
	if !res.CooldownUntil.IsZero() {
		t.Errorf("cooldown should be empty when remaining>0, got %v", res.CooldownUntil)
	}

	saved := repo.get(42)
	if saved["account_email"] != "u@x.com" || saved["image_account_plan"] != "plus" {
		t.Errorf("repo extra wrong: %v", saved)
	}
	if saved["image_quota_remaining"] != 7 {
		t.Errorf("quota_remaining: %v", saved["image_quota_remaining"])
	}
	if saved["image_cooldown_until"] != "" {
		t.Errorf("cooldown_until should be cleared when remaining>0: %v", saved["image_cooldown_until"])
	}
}

func checkProbeHeaders(t *testing.T, r *http.Request, wantPath string) bool {
	t.Helper()
	if got := r.Header.Get("authorization"); got != "Bearer tok" {
		t.Errorf("authorization=%q", got)
		return false
	}
	if got := r.Header.Get("x-openai-target-path"); got != wantPath {
		t.Errorf("target-path=%q want %q", got, wantPath)
		return false
	}
	return true
}

func TestAccountProbe_AuthFailedReturnsError(t *testing.T) {
	ResetProbeThrottleForTest()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	})
	mux.HandleFunc("/init", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	probe := NewAccountProbe(nil)
	probe.MeURL = srv.URL + "/me"
	probe.InitURL = srv.URL + "/init"

	_, err := probe.RefreshAccount(context.Background(), ProbeAccount{ID: 1, AccessToken: "tok"})
	if err == nil {
		t.Fatal("expect error")
	}
}

func TestAccountProbe_InitDegradedFallsBack(t *testing.T) {
	ResetProbeThrottleForTest()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"email":"a@b.com"}`))
	})
	mux.HandleFunc("/init", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	probe := NewAccountProbe(nil)
	probe.MeURL = srv.URL + "/me"
	probe.InitURL = srv.URL + "/init"

	res, err := probe.RefreshAccount(context.Background(), ProbeAccount{ID: 1, AccessToken: "tok"})
	if err != nil {
		t.Fatalf("should not fail when init 403 but me ok: %v", err)
	}
	if res.Email != "a@b.com" {
		t.Errorf("email=%q", res.Email)
	}
	if res.QuotaRemaining != -1 {
		t.Errorf("expect quota unknown -1, got %d", res.QuotaRemaining)
	}
}

func TestPlanFromJWT(t *testing.T) {
	// 构造一个手工 JWT 第二段：{"https://api.openai.com/auth":{"chatgpt_plan_type":"chatgptpro"}}
	// raw url-safe base64 编码（无 padding）：
	payload := `eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9wbGFuX3R5cGUiOiJjaGF0Z3B0cHJvIn19`
	tok := "header." + payload + ".sig"
	if got := planFromJWT(tok); got != "pro" {
		t.Errorf("planFromJWT=%q want pro", got)
	}
	if got := planFromJWT("invalid"); got != "" {
		t.Errorf("invalid token should yield empty, got %q", got)
	}
}

func TestNormalizePlan(t *testing.T) {
	cases := map[string]string{
		"chatgptplus":   "plus",
		"ChatGPT Pro":   "pro",
		"team":          "team",
		"ENTERPRISE":    "enterprise",
		"":              "free",
		"unknown_thing": "unknown_thing",
	}
	for in, want := range cases {
		if got := normalizePlan(in); got != want {
			t.Errorf("normalizePlan(%q)=%q want %q", in, got, want)
		}
	}
}

func TestExtractImageQuota_NoEntry(t *testing.T) {
	r := &ProbeResult{QuotaRemaining: -1}
	extractImageQuota([]any{
		map[string]any{"feature_name": "audio", "remaining": 5.0},
	}, r)
	if r.QuotaRemaining != -1 {
		t.Errorf("remaining should stay -1 when no image_gen entry")
	}
}

func TestExtractImageQuota_CooldownOnlyWhenExhausted(t *testing.T) {
	resetAt := time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)
	// remaining > 0: reset_after is just rolling-window reset, not a cooldown
	r := &ProbeResult{QuotaRemaining: -1}
	extractImageQuota([]any{
		map[string]any{
			"feature_name": "image_gen",
			"remaining":    3.0,
			"limit":        40.0,
			"reset_after":  resetAt.Format(time.RFC3339),
		},
	}, r)
	if !r.CooldownUntil.IsZero() {
		t.Errorf("cooldown should be zero when remaining>0, got %v", r.CooldownUntil)
	}

	// remaining == 0: now reset_after IS the cooldown
	r2 := &ProbeResult{QuotaRemaining: -1}
	extractImageQuota([]any{
		map[string]any{
			"feature_name": "image_gen",
			"remaining":    0.0,
			"limit":        40.0,
			"reset_after":  resetAt.Format(time.RFC3339),
		},
	}, r2)
	if !r2.CooldownUntil.Equal(resetAt) {
		t.Errorf("cooldown should equal reset_after when remaining=0, got %v", r2.CooldownUntil)
	}
}

func TestShouldProbeNow_Throttle(t *testing.T) {
	ResetProbeThrottleForTest()
	now := time.Now()
	if !ShouldProbeNow(99, now) {
		t.Fatal("first call should pass")
	}
	if ShouldProbeNow(99, now.Add(5*time.Second)) {
		t.Fatal("within throttle window should be denied")
	}
	if !ShouldProbeNow(99, now.Add(probeMinInterval+time.Second)) {
		t.Fatal("after throttle should pass")
	}
}

func TestMarkRateLimited_WritesCooldown(t *testing.T) {
	repo := &stubRepo{}
	probe := NewAccountProbe(repo)
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := probe.MarkRateLimited(context.Background(), 5, t0); err != nil {
		t.Fatal(err)
	}
	saved := repo.get(5)
	if saved["image_cooldown_until"] != t0.Format(time.RFC3339) {
		t.Errorf("cooldown=%v", saved["image_cooldown_until"])
	}
}
