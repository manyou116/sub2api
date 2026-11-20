//go:build unit

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestWebImageShadowBatchHTTPContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOpenAIOAuthHandler(nil, &stubAdminService{}, nil, nil, nil)
	router := gin.New()
	router.POST("/accounts/web-image-shadows", h.CreateWebImageShadows)
	for _, tc := range []struct {
		body  string
		want  int
		count int
	}{
		{`{"parent_ids":[10,20,10],"concurrency":2}`, http.StatusOK, 2},
		{`{"parent_ids":[10,20,10],"modes":["web","work"]}`, http.StatusOK, 4},
		{`{"parent_ids":[]}`, http.StatusBadRequest, 0},
		{`{"parent_ids":[10],"modes":["invalid"]}`, http.StatusBadRequest, 0},
		{`{"parent_ids":[10],"concurrency":0}`, http.StatusBadRequest, 0},
		{`{"parent_ids":[-1]}`, http.StatusBadRequest, 0},
		{`{"parent_ids":"10"}`, http.StatusBadRequest, 0},
	} {
		t.Run(tc.body, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/accounts/web-image-shadows", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, r)
			require.Equal(t, tc.want, w.Code)
			if tc.want == http.StatusOK {
				var response struct {
					Data service.WebImageShadowBatchResult `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.Equal(t, tc.count, response.Data.Created)
				require.Len(t, response.Data.Results, tc.count)
				require.EqualValues(t, 10, response.Data.Results[0].ParentID)
				require.EqualValues(t, 20, response.Data.Results[tc.count-1].ParentID)
				if tc.count == 4 {
					require.Equal(t, "work", response.Data.Results[1].Mode)
				}
				require.Equal(t, "created", response.Data.Results[0].Status)
				require.Equal(t, "web", response.Data.Results[0].Mode)
			}
		})
	}
}
