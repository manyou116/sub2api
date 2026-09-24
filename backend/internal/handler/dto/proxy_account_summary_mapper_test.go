package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProxyAccountSummaryFromServiceState(t *testing.T) {
	note := "operator note"
	for _, schedulable := range []bool{false, true} {
		src := &service.ProxyAccountSummary{
			ID: 17, Name: "account", Platform: "openai", Type: "oauth", Notes: &note,
			ProxyID: 9, Status: "active", Schedulable: schedulable,
		}
		got := ProxyAccountSummaryFromService(src)
		require.NotNil(t, got)
		raw, err := json.Marshal(got)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(raw, &fields))
		require.Equal(t, map[string]any{
			"id": float64(17), "name": "account", "platform": "openai", "type": "oauth", "notes": note,
			"proxy_id": float64(9), "status": "active", "schedulable": schedulable,
		}, fields)
	}
	require.Nil(t, ProxyAccountSummaryFromService(nil))
}
