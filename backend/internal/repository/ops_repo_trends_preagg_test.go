package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGetThroughputTrendUsesHourlyPreaggregation(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &opsRepository{db: db}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)

	mock.ExpectQuery("SELECT bucket_start, success_count").
		WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"bucket_start", "success_count", "error_count_total", "token_consumed"}).
			AddRow(start, int64(10), int64(2), int64(1200)).
			AddRow(start.Add(time.Hour), int64(20), int64(1), int64(2400)))
	mock.ExpectQuery("SELECT date_trunc\\('hour', created_at\\)").
		WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "switch_count"}))
	mock.ExpectQuery("WITH usage_totals AS").
		WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"platform", "request_count", "token_consumed"}).
			AddRow("openai", int64(33), int64(3600)))

	result, err := repo.GetThroughputTrend(context.Background(), &service.OpsDashboardFilter{
		StartTime: start,
		EndTime:   end,
		QueryMode: service.OpsQueryModePreagg,
	}, 3600)
	require.NoError(t, err)
	require.Len(t, result.Points, 2)
	require.EqualValues(t, 12, result.Points[0].RequestCount)
	require.EqualValues(t, 21, result.Points[1].RequestCount)
	require.Len(t, result.ByPlatform, 1)
	require.Equal(t, "openai", result.ByPlatform[0].Platform)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCanUseHourlyThroughputPreaggregation(t *testing.T) {
	groupID := int64(7)
	tests := []struct {
		name   string
		filter *service.OpsDashboardFilter
		bucket int
		want   bool
	}{
		{name: "long global window", filter: &service.OpsDashboardFilter{QueryMode: service.OpsQueryModeAuto}, bucket: 3600, want: true},
		{name: "short window", filter: &service.OpsDashboardFilter{QueryMode: service.OpsQueryModeAuto}, bucket: 300, want: false},
		{name: "raw mode", filter: &service.OpsDashboardFilter{QueryMode: service.OpsQueryModeRaw}, bucket: 3600, want: false},
		{name: "filtered group", filter: &service.OpsDashboardFilter{QueryMode: service.OpsQueryModeAuto, GroupID: &groupID}, bucket: 3600, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, canUseHourlyThroughputPreaggregation(tc.filter, tc.bucket))
		})
	}
}
