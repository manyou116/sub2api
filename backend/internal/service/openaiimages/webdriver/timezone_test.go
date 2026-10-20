package webdriver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestWebImageTimezoneDSTAndOffsets(t *testing.T) {
	for _, tt := range []struct {
		zone, utc, dateSuffix string
		offset                int
	}{
		{"America/New_York", "2026-01-15T12:00:00Z", "GMT-0500 (Eastern Standard Time)", 300},
		{"America/New_York", "2026-07-15T12:00:00Z", "GMT-0400 (Eastern Daylight Time)", 240},
		{"America/New_York", "2026-03-08T06:59:59Z", "GMT-0500 (Eastern Standard Time)", 300},
		{"America/New_York", "2026-03-08T07:00:00Z", "GMT-0400 (Eastern Daylight Time)", 240},
		{"America/New_York", "2026-11-01T05:59:59Z", "GMT-0400 (Eastern Daylight Time)", 240},
		{"America/New_York", "2026-11-01T06:00:00Z", "GMT-0500 (Eastern Standard Time)", 300},
		{"America/Los_Angeles", "2026-01-15T12:00:00Z", "GMT-0800 (Pacific Standard Time)", 480},
		{"America/Los_Angeles", "2026-07-15T12:00:00Z", "GMT-0700 (Pacific Daylight Time)", 420},
		{"UTC", "2026-07-15T12:00:00Z", "GMT+0000 (Coordinated Universal Time)", 0},
		{"Asia/Shanghai", "2026-07-15T12:00:00Z", "GMT+0800 (China Standard Time)", -480},
		{"Asia/Kathmandu", "2026-07-15T12:00:00Z", "GMT+0545 (+0545)", -345},
	} {
		t.Run(tt.zone+"/"+tt.utc, func(t *testing.T) {
			location, err := time.LoadLocation(tt.zone)
			require.NoError(t, err)
			now, err := time.Parse(time.RFC3339, tt.utc)
			require.NoError(t, err)
			name, offset := webImageTimezoneFields(location, now)
			require.Equal(t, tt.zone, name)
			require.Equal(t, tt.offset, offset)
			require.True(t, strings.HasSuffix(browserDateString(now.In(location)), tt.dateSuffix))
		})
	}
}

func TestWebImageTimezoneEnvironment(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"", "America/New_York"}, {"  ", "America/New_York"},
		{"UTC", "UTC"}, {" America/Los_Angeles ", "America/Los_Angeles"},
	} {
		t.Run(tt.input, func(t *testing.T) {
			t.Setenv(webImageTimezoneEnv, tt.input)
			location, err := loadWebImageTimezone()
			require.NoError(t, err)
			require.Equal(t, tt.want, location.String())
		})
	}
	for _, invalid := range []string{"Invalid/Timezone", "Local"} {
		t.Run(invalid, func(t *testing.T) {
			t.Setenv(webImageTimezoneEnv, invalid)
			driver := NewDriver(func(string) (*req.Client, error) {
				t.Fatal("invalid timezone must fail before initializing an upstream session")
				return nil, nil
			})
			auth := Auth{AccessToken: "test-token"}
			_, err := driver.ProbeQuota(context.Background(), auth)
			require.ErrorContains(t, err, webImageTimezoneEnv)
			_, err = driver.Generate(context.Background(), auth, GenerateRequest{Model: "auto"})
			require.ErrorContains(t, err, webImageTimezoneEnv)
			_, err = driver.Edit(context.Background(), auth, GenerateRequest{Model: "auto"})
			require.ErrorContains(t, err, webImageTimezoneEnv)
		})
	}
}

func TestWebImageTimezoneOutboundProbeGenerationAndEdit(t *testing.T) {
	for _, zone := range []string{"", "UTC", "America/Los_Angeles", "Asia/Shanghai"} {
		for _, edit := range []bool{false, true} {
			name := zone + "/generation"
			if edit {
				name = zone + "/edit"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv(webImageTimezoneEnv, zone)
				wantZone := zone
				if wantZone == "" {
					wantZone = "America/New_York"
				}
				location, err := time.LoadLocation(wantZone)
				require.NoError(t, err)
				_, seconds := time.Now().In(location).Zone()
				checkProof := func(value any, prefix string) {
					token, ok := value.(string)
					require.True(t, ok)
					require.True(t, strings.HasPrefix(token, prefix))
					body, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(token, prefix))
					require.NoError(t, err)
					var config []any
					require.NoError(t, json.Unmarshal(body, &config))
					require.Contains(t, config[1], time.Now().In(location).Format("GMT-0700"))
					require.Equal(t, "bound-agent", config[4])
				}
				calls := map[string]int{}
				driver := NewDriver(func(string) (*req.Client, error) {
					client := req.C()
					client.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
						return func(r *http.Request) (*http.Response, error) {
							calls[r.URL.Path]++
							body := `{}`
							switch r.URL.Path {
							case "/backend-api/sentinel/chat-requirements/prepare", "/backend-api/sentinel/chat-requirements/finalize":
								var payload map[string]any
								require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
								if strings.HasSuffix(r.URL.Path, "/prepare") {
									checkProof(payload["p"], "gAAAAAC")
									body = `{"prepare_token":"prepare","proofofwork":{"required":true,"seed":"mock-seed","difficulty":"ff"}}`
								} else {
									checkProof(payload["proofofwork"], "gAAAAAB")
									body = `{"token":"requirements"}`
								}
							case "/backend-api/conversation/init", "/backend-api/f/conversation/prepare", "/backend-api/f/conversation":
								var payload map[string]any
								require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
								require.Equal(t, wantZone, payload["timezone"])
								require.Equal(t, float64(-seconds/60), payload["timezone_offset_min"])
								if r.URL.Path == "/backend-api/conversation/init" {
									body = `{"limits_progress":[{"feature_name":"image_gen","remaining":3}]}`
								} else {
									require.Equal(t, "auto", payload["model"])
									require.NotContains(t, payload, "thinking_effort")
									if strings.HasSuffix(r.URL.Path, "/prepare") {
										body = `{"conduit_token":"fresh-conduit"}`
									} else {
										body = "data: {\"conversation_id\":\"mock\",\"asset_pointer\":\"file-service://file_output\"}\n\n"
									}
								}
							case "/backend-api/files":
								body = `{"file_id":"file_input","upload_url":"https://mock.invalid/upload"}`
							case "/upload", "/backend-api/files/file_input/uploaded":
							case "/backend-api/files/download/file_output":
								body = `{"download_url":"https://mock.invalid/image"}`
							case "/image":
								body = string(protocolPNG(t))
							default:
								t.Errorf("unexpected path %s", r.URL.Path)
							}
							return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
						}
					})
					return client, nil
				})
				driver.KeepConversation = true
				auth := Auth{AccessToken: "test-token", UserAgent: "bound-agent"}
				_, err = driver.ProbeQuota(context.Background(), auth)
				require.NoError(t, err)
				request := GenerateRequest{Model: "auto", Prompt: "draw", ThinkingEffort: "extended", N: 1}
				var result *GenerateResult
				if edit {
					request.Images = []InputImage{{FileName: "input.png", Data: []byte("input")}}
					result, err = driver.Edit(context.Background(), auth, request)
				} else {
					result, err = driver.Generate(context.Background(), auth, request)
				}
				require.NoError(t, err)
				require.Len(t, result.Data, 1)
				for _, path := range []string{"/backend-api/conversation/init", "/backend-api/f/conversation/prepare", "/backend-api/f/conversation", "/image"} {
					require.Equal(t, 1, calls[path], path)
				}
			})
		}
	}
}
