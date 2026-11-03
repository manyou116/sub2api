package repository

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func sourceUserJSON(user *service.SourceUser) sql.NullString {
	if user == nil {
		return sql.NullString{}
	}
	data, err := json.Marshal(user)
	return sql.NullString{String: string(data), Valid: err == nil}
}

func appendSourceUserQueryFilter(query string, args []any, sourceID, userID, alias string) (string, []any) {
	conditions, args := appendSourceUserWhere(nil, args, sourceID, userID, alias)
	if len(conditions) > 0 {
		query += " AND " + strings.Join(conditions, " AND ")
	}
	return query, args
}

func decodeSourceUser(raw sql.NullString) *service.SourceUser {
	if !raw.Valid {
		return nil
	}
	var user service.SourceUser
	if json.Unmarshal([]byte(raw.String), &user) != nil {
		return nil
	}
	return &user
}

// alias is a repository-owned SQL identifier. Values always use parameters.
func appendSourceUserWhere(conditions []string, args []any, sourceID, userID, alias string) ([]string, []any) {
	column := "source_user"
	if alias != "" {
		column = alias + "." + column
	}
	if sourceID == "" && userID == "" {
		return conditions, args
	}
	conditions = append(conditions, column+" IS NOT NULL")
	for _, filter := range []struct{ field, value string }{{"source_id", sourceID}, {"user_id", userID}} {
		if filter.value == "" {
			continue
		}
		conditions = append(conditions, fmt.Sprintf("%s->>'%s' = $%d", column, filter.field, len(args)+1))
		args = append(args, filter.value)
	}
	return conditions, args
}
