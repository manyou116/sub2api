//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type qualitySampleRepo struct {
	AccountRepository
	samples []OpenAIQualitySample
	calls   int
}

func (r *qualitySampleRepo) RecentOpenAIQualitySamples(context.Context, time.Time) ([]OpenAIQualitySample, error) {
	r.calls++
	return r.samples, nil
}

func TestScheduledQualityVerdictAndScore(t *testing.T) {
	plan := &ScheduledTestPlan{QualityPrompt: "Reply only 4", QualityExpected: "4", QualityPriority: true}
	require.NoError(t, validateScheduledQuality(plan))
	require.Equal(t, "pass", scheduledQualityVerdict("4", &ScheduledTestResult{Status: "success", ResponseText: "4\n"}))
	require.Equal(t, "fail", scheduledQualityVerdict("4", &ScheduledTestResult{Status: "success", ResponseText: "5"}))
	require.Empty(t, scheduledQualityVerdict("4", &ScheduledTestResult{Status: "failed", ResponseText: "4"}))
	repo := &qualitySampleRepo{samples: []OpenAIQualitySample{
		{AccountID: 1, Model: "a", Passed: 3, Total: 3},
		{AccountID: 2, Model: "a", Passed: 0, Total: 3, Isolated: true},
		{AccountID: 3, Model: "a", Passed: 1, Total: 1},
	}}
	s := &OpenAIGatewayService{accountRepo: repo}
	accounts := []openAIAccountCandidateScore{{account: &Account{ID: 1}}, {account: &Account{ID: 2}}, {account: &Account{ID: 3}}}
	require.Equal(t, map[int64]float64{1: 1, 2: -1}, s.scheduledQualityScores(context.Background(), accounts, "a"))
	require.Empty(t, s.scheduledQualityScores(context.Background(), accounts, "b"))
	require.Equal(t, map[int64]struct{}{2: {}, 4: {}}, s.excludeQualityIsolatedAccounts(context.Background(), "a", map[int64]struct{}{4: {}}))
	require.Empty(t, s.excludeQualityIsolatedAccounts(context.Background(), "b", nil))
	require.Equal(t, 1, repo.calls)
}
