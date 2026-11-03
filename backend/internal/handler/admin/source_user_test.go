package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceUserUsageFiltersAndInvalidScope(t *testing.T) {
	repo := &adminUsageRepoCapture{}
	router := newAdminUsageRequestTypeTestRouter(repo)
	for _, path := range []string{"/admin/usage", "/admin/usage/stats"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+"?source_id=factory&source_user_id=123", nil))
		require.Equal(t, http.StatusOK, rec.Code)
	}
	require.Equal(t, "factory", repo.listFilters.SourceID)
	require.Equal(t, "123", repo.listFilters.SourceUserID)
	require.Equal(t, "factory", repo.statsFilters.SourceID)
	require.Equal(t, "123", repo.statsFilters.SourceUserID)
	for _, query := range []string{"source_user_id=123", "source_id=bad%20site", "source_id=factory&source_user_id=bad%20user"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/usage?"+query, nil))
		require.Equal(t, http.StatusBadRequest, rec.Code)
	}
	other := repo.statsFilters
	other.SourceUserID = "456"
	require.NotEqual(t, usageStatsCacheKey(repo.statsFilters), usageStatsCacheKey(other))
}

func TestSourceUserDashboardFiltersAndCachesStayIsolated(t *testing.T) {
	repo := &dashboardUsageRepoCapture{}
	router := newDashboardRequestTypeTestRouter(repo)
	for _, path := range []string{"/admin/dashboard/models", "/admin/dashboard/trend", "/admin/dashboard/groups"} {
		for _, user := range []string{"source-test-123", "source-test-456"} {
			repo.sourceID, repo.sourceUserID = "", ""
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+"?source_id=source-test&source_user_id="+user, nil))
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, "source-test", repo.sourceID)
			require.Equal(t, user, repo.sourceUserID)
		}
	}
}
