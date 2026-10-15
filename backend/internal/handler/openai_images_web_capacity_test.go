//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/imroc/req/v3"
	"github.com/redis/go-redis/v9" //nolint:depguard // Test-only client verifies shared image slots through the real handler.
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type webImagesCapacityRepo struct {
	openAIImagesFailoverAccountRepo
}

func (r webImagesCapacityRepo) UpdateExtra(context.Context, int64, map[string]any) error { return nil }
func (r webImagesCapacityRepo) ListSchedulableByGroupID(context.Context, int64) ([]service.Account, error) {
	return r.accounts, nil
}
func (r webImagesCapacityRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]service.Account, error) {
	return r.accounts, nil
}
func (r webImagesCapacityRepo) ListActiveAllowingTextRateLimitByGroupIDAndPlatforms(context.Context, int64, []string) ([]service.Account, error) {
	return r.accounts, nil
}
func (r webImagesCapacityRepo) ListActiveAllowingTextRateLimitByPlatforms(context.Context, []string) ([]service.Account, error) {
	return r.accounts, nil
}

func TestImagesWebCapacityReselect(t *testing.T) {
	for _, endpoint := range []string{"generations", "edits"} {
		for _, scenario := range []struct {
			name                       string
			count, faults, raceAccount int
			busy                       []int64
			status                     int
		}{
			{name: "A busy B free", count: 2, busy: []int64{1}, status: 200},
			{name: "A loses atomic race B free", count: 2, raceAccount: 1, status: 200},
			{name: "five faults then race then healthy", count: 7, faults: 5, raceAccount: 6, status: 200},
			{name: "all busy", count: 2, busy: []int64{1, 2}, status: 503},
			{name: "race exhausts capacity", count: 1, raceAccount: 1, status: 503},
		} {
			t.Run(endpoint+"/"+scenario.name, func(t *testing.T) {
				t.Setenv("SUB2API_SCHEDULER_BOUNDED_PROBE", "true")
				gin.SetMode(gin.TestMode)
				groupID := int64(32)
				accounts := make([]service.Account, scenario.count)
				for i := range accounts {
					accounts[i] = service.Account{ID: int64(i + 1), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
						Status: service.StatusActive, Schedulable: true, Concurrency: 10, Priority: i, GroupIDs: []int64{groupID},
						Credentials: map[string]any{"access_token": fmt.Sprintf("test-%d", i+1)},
						Extra:       map[string]any{"openai_web_images": map[string]any{"enabled": true, "max_inflight": 1, "require_download_attachment": false}},
					}
				}
				repo := webImagesCapacityRepo{openAIImagesFailoverAccountRepo{accounts: accounts}}
				cfg := &config.Config{RunMode: config.RunModeSimple, Gateway: config.GatewayConfig{OpenAIWebImages: config.OpenAIWebImagesConfig{
					InflightBackend: "redis", DefaultMaxInflight: 3, UnknownQuotaPolicy: "optimistic", RedisKeyPrefix: "test:webimg:", InflightTTLSeconds: 900,
					DefaultUpstreamModel: "auto",
				}}}
				mr := miniredis.RunT(t)
				client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
				t.Cleanup(func() { _ = client.Close() })
				web := service.NewOpenAIWebImagesService(cfg, client, repo)
				for _, id := range scenario.busy {
					ok, err := web.Acquire(context.Background(), id, 1, "other-request")
					require.NoError(t, err)
					require.True(t, ok)
				}
				upstreamCalls := make(map[int]int)
				web.Driver().KeepConversation = true
				web.Driver().ClientFactory = func(string) (*req.Client, error) {
					c := req.C()
					c.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
						return func(r *http.Request) (*http.Response, error) {
							id, _ := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer test-"))
							if id > 0 {
								upstreamCalls[id]++
							}
							body := `{}`
							status := 200
							if id > 0 && id <= scenario.faults {
								status, body = 403, "<html><head><style global></style></head><body>blocked</body></html>"
							} else {
								if r.URL.Path == "/backend-api/f/conversation/prepare" || r.URL.Path == "/backend-api/f/conversation" {
									payload, err := io.ReadAll(r.Body)
									require.NoError(t, err)
									require.Equal(t, "auto", gjson.GetBytes(payload, "model").String())
								}
								switch {
								case strings.HasSuffix(r.URL.Path, "/conversation/init"):
									body = `{"limits_progress":[{"feature_name":"image_gen","remaining":3}]}`
								case strings.Contains(r.URL.Path, "/sentinel/chat-requirements/"):
									body = `{"token":"mock","proofofwork":{"required":false}}`
								case r.URL.Path == "/backend-api/files":
									body = `{"file_id":"file_input","upload_url":"https://mock.invalid/upload"}`
								case r.URL.Path == "/backend-api/f/conversation":
									body = "data: {\"conversation_id\":\"test-conversation\",\"asset_pointer\":\"file-service://file_00000000aaaaaaaaaaaaaaaaaaaaaaaa\"}\n\n"
								case strings.HasSuffix(r.URL.Path, "/download"):
									body = `{"download_url":"https://mock.invalid/image"}`
								case r.URL.Path == "/image":
									body = "image-bytes"
								}
							}
							return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
						}
					})
					return c, nil
				}
				gw := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				gw.SetOpenAIWebImagesService(web)
				billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
				t.Cleanup(billing.Stop)
				h := NewOpenAIGatewayHandler(gw, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
				h.maxAccountSwitches = 5
				core, logs := observer.New(zap.DebugLevel)
				selected := 0
				raced := false
				log := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
					if e.Message == "openai.images.account_selected" {
						selected++
						if selected == scenario.raceAccount {
							raced = true
							ok, err := web.Acquire(context.Background(), int64(scenario.raceAccount), 1, "racing-request")
							require.NoError(t, err)
							require.True(t, ok)
						}
					}
					return nil
				}))
				var body bytes.Buffer
				contentType := "application/json"
				if endpoint == "edits" {
					writer := multipart.NewWriter(&body)
					require.NoError(t, writer.WriteField("model", "gpt-image-2"))
					require.NoError(t, writer.WriteField("prompt", "draw"))
					part, err := writer.CreateFormFile("image", "input.png")
					require.NoError(t, err)
					_, err = part.Write([]byte("input image"))
					require.NoError(t, err)
					require.NoError(t, writer.Close())
					contentType = writer.FormDataContentType()
				} else {
					body.WriteString(`{"model":"gpt-image-2","prompt":"draw"}`)
				}
				c, recorder := newWebCapacityContext(&body, contentType, endpoint)
				c.Request = c.Request.WithContext(logger.IntoContext(context.Background(), log))
				c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 99, GroupID: &groupID, Group: &service.Group{ID: groupID, AllowImageGeneration: true}, User: &service.User{ID: 100}})
				c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
				h.Images(c)
				if recorder.Code != scenario.status {
					for _, entry := range logs.All() {
						if entry.Level >= zap.WarnLevel {
							t.Logf("%s: %v", entry.Message, entry.ContextMap())
						}
					}
				}
				require.Equal(t, scenario.status, recorder.Code, recorder.Body.String())
				if scenario.raceAccount > 0 {
					require.True(t, raced, "exercise the check/acquire race")
					require.Zero(t, upstreamCalls[scenario.raceAccount], "local capacity must never reach upstream")
					require.Len(t, logs.FilterMessage("openai.images.local_capacity_reselect").All(), 1)
				}
				for _, id := range scenario.busy {
					require.Zero(t, upstreamCalls[int(id)])
				}
				if scenario.status == 200 {
					require.Equal(t, "aW1hZ2UtYnl0ZXM=", gjson.GetBytes(recorder.Body.Bytes(), "data.0.b64_json").String())
					require.False(t, web.IsInflightFull(context.Background(), &accounts[len(accounts)-1]))
				} else {
					require.Contains(t, recorder.Body.String(), "capacity")
				}
				require.Len(t, logs.FilterMessage("openai.images.upstream_failover_switching").All(), scenario.faults, "only actual upstream failures consume switches")
				for id := 1; id <= scenario.faults; id++ {
					require.Equal(t, 1, upstreamCalls[id])
					require.True(t, web.IsWebRateLimited(context.Background(), int64(id)))
				}
			})
		}
	}
}

func newWebCapacityContext(body *bytes.Buffer, contentType, endpoint string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/"+endpoint, body)
	c.Request.Header.Set("Content-Type", contentType)
	return c, recorder
}
