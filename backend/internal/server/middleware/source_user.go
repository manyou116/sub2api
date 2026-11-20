package middleware

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// SourceUser accepts metadata only from an explicitly bound, authenticated key.
// Remove attribution headers before any provider can copy inbound headers.
func SourceUser(settings *service.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		source, encoded := c.GetHeader("X-Sub2api-Source"), c.GetHeader("X-Sub2api-User")
		c.Request.Header.Del("X-Sub2api-Source")
		c.Request.Header.Del("X-Sub2api-User")
		if !service.SourceUserIDPattern.MatchString(source) || len(encoded) == 0 || len(encoded) > 2048 {
			c.Next()
			return
		}
		key, ok := GetAPIKeyFromContext(c)
		if !ok {
			c.Next()
			return
		}
		sources, err := settings.GetSourceUserIntegrations(c.Request.Context())
		if err != nil {
			c.Next()
			return
		}
		for _, integration := range sources {
			if integration.ID != source || !slices.Contains(integration.APIKeyIDs, key.ID) {
				continue
			}
			data, err := base64.RawURLEncoding.DecodeString(encoded)
			var identity service.SourceUser
			if err != nil || !utf8.Valid(data) || json.Unmarshal(data, &identity) != nil || !service.SourceUserIDPattern.MatchString(identity.UserID) || len(identity.Username) > 256 || len(identity.TokenID) > 64 || len(identity.RequestID) > 128 || strings.ContainsAny(identity.Username, "\r\n\x00") {
				break
			}
			identity.SourceID, identity.SourceName = integration.ID, integration.Name
			c.Request = c.Request.WithContext(service.WithSourceUser(c.Request.Context(), &identity))
			break
		}
		c.Next()
	}
}
