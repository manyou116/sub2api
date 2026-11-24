package service

import (
	"context"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func validateScheduledQuality(plan *ScheduledTestPlan) error {
	plan.QualityPrompt = strings.TrimSpace(plan.QualityPrompt)
	plan.QualityExpected = strings.TrimSpace(plan.QualityExpected)
	if (plan.QualityPrompt == "") != (plan.QualityExpected == "") ||
		len(plan.QualityPrompt) > 4096 || len(plan.QualityExpected) > 2048 ||
		((plan.QualityPriority || plan.QualityIsolationFailures > 0) && plan.QualityExpected == "") {
		return fmt.Errorf("quality testing requires a question and expected answer within the size limits")
	}
	if plan.QualityIsolationFailures < 0 || plan.QualityIsolationFailures > 8 {
		return fmt.Errorf("quality isolation requires 0 to 8 consecutive failed results")
	}
	return nil
}

func scheduledQualityVerdict(expected string, result *ScheduledTestResult) string {
	if expected == "" || result.Status != "success" || strings.TrimSpace(result.ResponseText) == "" {
		return ""
	}
	if strings.TrimSpace(result.ResponseText) == expected {
		return "pass"
	}
	return "fail"
}

type OpenAIQualitySample struct {
	AccountID int64
	Model     string
	Passed    int
	Total     int
	Isolated  bool
}

type scheduledOpenAIQualityReader interface {
	RecentOpenAIQualitySamples(context.Context, time.Time) ([]OpenAIQualitySample, error)
}

type scheduledQualityKey struct {
	id    int64
	model string
}

type scheduledQualityScoresSnapshot struct {
	scores    map[scheduledQualityKey]float64
	blocked   map[string][]int64
	expiresAt time.Time
}

type scheduledOpenAIQualityCache struct {
	value atomic.Pointer[scheduledQualityScoresSnapshot]
	load  sync.Mutex
}

func (s *OpenAIGatewayService) scheduledQualityScores(ctx context.Context, candidates []openAIAccountCandidateScore, model string) map[int64]float64 {
	snapshot := s.scheduledQualitySnapshot(ctx, model)
	if snapshot == nil || len(snapshot.scores) == 0 {
		return nil
	}
	result := make(map[int64]float64)
	for _, candidate := range candidates {
		key := scheduledQualityKey{candidate.account.ID, model}
		if score, present := snapshot.scores[key]; present {
			result[key.id] = score
		}
	}
	return result
}

func (s *OpenAIGatewayService) scheduledQualitySnapshot(ctx context.Context, model string) *scheduledQualityScoresSnapshot {
	repo, ok := s.accountRepo.(scheduledOpenAIQualityReader)
	if !ok || model == "" {
		return nil
	}
	snapshot := s.scheduledQuality.value.Load()
	if snapshot == nil || time.Now().After(snapshot.expiresAt) {
		if s.scheduledQuality.load.TryLock() {
			snapshot = s.loadScheduledQuality(ctx, repo)
			s.scheduledQuality.load.Unlock()
		}
	}
	if snapshot == nil || time.Now().After(snapshot.expiresAt) {
		return nil
	}
	return snapshot
}

func (s *OpenAIGatewayService) excludeQualityIsolatedAccounts(ctx context.Context, model string, excluded map[int64]struct{}) map[int64]struct{} {
	snapshot := s.scheduledQualitySnapshot(ctx, model)
	if snapshot == nil || len(snapshot.blocked[model]) == 0 {
		return excluded
	}
	next := make(map[int64]struct{}, len(excluded)+len(snapshot.blocked[model]))
	for id := range excluded {
		next[id] = struct{}{}
	}
	for _, id := range snapshot.blocked[model] {
		next[id] = struct{}{}
	}
	return next
}

func (s *OpenAIGatewayService) loadScheduledQuality(ctx context.Context, reader scheduledOpenAIQualityReader) *scheduledQualityScoresSnapshot {
	lookup, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	now := time.Now()
	snapshot := &scheduledQualityScoresSnapshot{expiresAt: now.Add(30 * time.Second)}
	samples, err := reader.RecentOpenAIQualitySamples(lookup, now.Add(-24*time.Hour))
	if err == nil {
		snapshot.scores = make(map[scheduledQualityKey]float64, len(samples))
		snapshot.blocked = make(map[string][]int64)
		for _, sample := range samples {
			if sample.Isolated {
				snapshot.blocked[sample.Model] = append(snapshot.blocked[sample.Model], sample.AccountID)
			}
			if sample.Total >= 3 {
				// A small additive score preserves hard eligibility and capacity checks.
				snapshot.scores[scheduledQualityKey{sample.AccountID, sample.Model}] = 2*float64(sample.Passed)/float64(sample.Total) - 1
			}
		}
	} else {
		snapshot.expiresAt = now.Add(5 * time.Second)
	}
	s.scheduledQuality.value.Store(snapshot)
	return snapshot
}

type qualityPlanClaimer interface {
	ClaimQualityPlan(context.Context, int64, int64, time.Time, time.Time) (bool, error)
	FinishQualityPlan(context.Context, int64, int64, time.Time, time.Time) error
}

func (s *ScheduledTestRunnerService) runQualityPlan(ctx context.Context, plan *ScheduledTestPlan) {
	claims, ok := s.planRepo.(qualityPlanClaimer)
	if !ok || plan.NextRunAt == nil {
		return
	}
	next, err := computeNextRun(plan.CronExpression, time.Now())
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "quality plan %d schedule: %v", plan.ID, err)
		return
	}
	claimed, err := claims.ClaimQualityPlan(ctx, plan.ID, plan.QualityRevision, *plan.NextRunAt, next)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "quality plan %d claim: %v", plan.ID, err)
		return
	}
	if !claimed {
		return
	}
	result, err := s.accountTestSvc.runTestBackgroundWithPrompt(ctx, plan.AccountID, plan.ModelID, plan.QualityPrompt)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "quality plan %d probe failed: %v", plan.ID, err)
		return
	}
	result.QualityRevision = plan.QualityRevision
	result.QualityVerdict = scheduledQualityVerdict(plan.QualityExpected, result)
	if err = s.scheduledSvc.SaveResult(ctx, plan.ID, plan.MaxResults, result); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "quality plan %d persist result: %v", plan.ID, err)
		return
	}
	if err = claims.FinishQualityPlan(ctx, plan.ID, plan.QualityRevision, next, result.FinishedAt); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "quality plan %d finish: %v", plan.ID, err)
	}
	if plan.AutoRecover && result.QualityVerdict == "pass" {
		current, err := s.planRepo.GetByID(ctx, plan.ID)
		if err == nil && current != nil && current.Enabled && current.AutoRecover && current.QualityRevision == plan.QualityRevision {
			s.tryRecoverAccount(ctx, plan.AccountID, plan.ID)
		}
	}
}
