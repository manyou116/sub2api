package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"strings"
)

func parseSourceUserFilter(c *gin.Context) (string, string, bool) {
	sourceID, userID := strings.TrimSpace(c.Query("source_id")), strings.TrimSpace(c.Query("source_user_id"))
	if (sourceID != "" && !service.SourceUserIDPattern.MatchString(sourceID)) || (userID != "" && !service.SourceUserIDPattern.MatchString(userID)) || (userID != "" && sourceID == "") {
		response.BadRequest(c, "Source user filters require a valid source_id and optional source_user_id")
		return "", "", false
	}
	return sourceID, userID, true
}
