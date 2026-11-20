package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

var (
	ErrCapacityMonitorNotFound = errors.New("group not found")
	ErrCapacityMonitorBudget   = errors.New("group capacity monitor query budget exceeded")
)

const CapacityMonitorModelVersion = "global-text-usd-v1"

type CapacityMonitorInventory struct {
	MemberCount            int `json:"member_count"`
	EligibleCount          int `json:"eligible_count"`
	SchedulableCount       int `json:"schedulable_count"`
	ModeledCount           int `json:"modeled_count"`
	DirectCount            int `json:"direct_count"`
	EstimatedCount         int `json:"estimated_count"`
	StaleCount             int `json:"stale_count"`
	MissingWindowCount     int `json:"missing_window_count"`
	SharedCount            int `json:"shared_count"`
	HistoryAccountCount    int `json:"history_account_count"`
	UnknownIdentityCount   int `json:"unknown_identity_count"`
	ProvenanceUnknownCount int `json:"provenance_unknown_count"`
	StateExcludedCount     int `json:"state_excluded_count"`
	DuplicateCount         int `json:"duplicate_count"`
}

type CapacityMonitorQuality struct {
	HistoryCompleteness string     `json:"history_completeness"`
	WindowProvenance    string     `json:"window_provenance"`
	IdentityQuality     string     `json:"identity_quality"`
	FreshestSampleAt    *time.Time `json:"freshest_sample_at"`
	OldestSampleAt      *time.Time `json:"oldest_sample_at"`
	UnclassifiedCost    float64    `json:"unclassified_cost"`
}

type CapacityMonitorHistory struct {
	StartAt            time.Time `json:"start_at"`
	EndAt              time.Time `json:"end_at"`
	Complete           bool      `json:"complete"`
	PoolAccountCost    float64   `json:"pool_account_cost"`
	GroupRouteCost     float64   `json:"group_route_cost"`
	PoolViaGroupCost   float64   `json:"pool_via_group_cost"`
	PoolOtherCost      float64   `json:"pool_other_cost"`
	PoolRequests       int64     `json:"pool_requests"`
	GroupRouteRequests int64     `json:"group_route_requests"`
}

type CapacityMonitorDemand struct {
	Key         string   `json:"key"`
	Hours       float64  `json:"hours"`
	Cost        float64  `json:"cost"`
	RatePerHour *float64 `json:"rate_per_hour"`
	Available   bool     `json:"available"`
	Reason      string   `json:"reason"`
}

type CapacityMonitorPoint struct {
	At                time.Time `json:"at"`
	Hours             float64   `json:"hours"`
	Capacity          *float64  `json:"capacity"`
	DirectCapacity    *float64  `json:"direct_capacity"`
	EstimatedCapacity *float64  `json:"estimated_capacity"`
	AccountCount      int       `json:"account_count"`
}

type CapacityMonitorMilestone struct {
	Hours    float64  `json:"hours"`
	Capacity *float64 `json:"capacity"`
	Demand   *float64 `json:"demand"`
	Gap      *float64 `json:"gap"`
}

type CapacityMonitorScenario struct {
	Key              string                     `json:"key"`
	FirstShortfallAt *time.Time                 `json:"first_shortfall_at"`
	MinimumGap       *float64                   `json:"minimum_gap"`
	Milestones       []CapacityMonitorMilestone `json:"milestones"`
}

type CapacityMonitorSharedUsage struct {
	GroupID     *int64    `json:"group_id"`
	GroupName   string    `json:"group_name"`
	Cost        float64   `json:"cost"`
	BucketStart time.Time `json:"-"`
}

type CapacityMonitorResponse struct {
	GroupID               int64                        `json:"group_id"`
	GroupName             string                       `json:"group_name"`
	AsOf                  time.Time                    `json:"as_of"`
	Timezone              string                       `json:"timezone"`
	Currency              string                       `json:"currency"`
	FeeBasis              string                       `json:"fee_basis"`
	ModelVersion          string                       `json:"model_version"`
	Scope                 string                       `json:"scope"`
	MembershipAsOf        time.Time                    `json:"membership_as_of"`
	MembershipFingerprint string                       `json:"membership_fingerprint"`
	Inventory             CapacityMonitorInventory     `json:"inventory"`
	Quality               CapacityMonitorQuality       `json:"quality"`
	History               []CapacityMonitorHistory     `json:"history"`
	DemandBases           []CapacityMonitorDemand      `json:"demand_bases"`
	CapacityPoints        []CapacityMonitorPoint       `json:"capacity_points"`
	Scenarios             []CapacityMonitorScenario    `json:"scenarios"`
	SharedUsage           []CapacityMonitorSharedUsage `json:"shared_usage"`
	Limitations           []string                     `json:"limitations"`
	Cache                 struct {
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"cache"`
}

// These projections never contain tokens, cookies or API keys. IdentityKey is a
// private hash of the provider account/user pair, never sent to the browser.
type CapacityMonitorAccount struct {
	ID                                                                    int64
	Platform, Type, QuotaDimension, Status, Plan, IdentityKey, TempReason string
	ParentID                                                              *int64
	Schedulable, AutoPauseOnExpired                                       bool
	ExpiresAt, RateResetAt, OverloadUntil, TempUntil                      *time.Time
	Extra                                                                 map[string]any
	Shared                                                                bool
	DuplicateIdentity                                                     bool
	Window5Cost, Window7Cost, Post5Cost, Post7Cost                        float64
}

type GroupCapacityMonitorSnapshot struct {
	AsOf             time.Time
	GroupName        string
	Accounts         []CapacityMonitorAccount
	History          []CapacityMonitorHistory
	SharedUsage      []CapacityMonitorSharedUsage
	UnclassifiedCost float64
}

type GroupCapacityMonitorRepository interface {
	ReadSnapshot(context.Context, int64, time.Duration) (*GroupCapacityMonitorSnapshot, error)
}

type GroupCapacityMonitorService struct {
	repo GroupCapacityMonitorRepository
}

func NewGroupCapacityMonitorService(repo GroupCapacityMonitorRepository) *GroupCapacityMonitorService {
	return &GroupCapacityMonitorService{repo: repo}
}

func CapacityMonitorDuration(value string) (time.Duration, bool) {
	switch value {
	case "24h":
		return 24 * time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	case "15d":
		return 15 * 24 * time.Hour, true
	}
	return 0, false
}

func (s *GroupCapacityMonitorService) Get(ctx context.Context, groupID int64, history, horizon time.Duration) (*CapacityMonitorResponse, error) {
	if groupID <= 0 || history <= 0 || history > 15*24*time.Hour || horizon <= 0 || horizon > 15*24*time.Hour {
		return nil, fmt.Errorf("invalid capacity monitor range")
	}
	queryHistory := history
	if queryHistory < 7*24*time.Hour {
		queryHistory = 7 * 24 * time.Hour
	}
	snapshot, err := s.repo.ReadSnapshot(ctx, groupID, queryHistory)
	if err != nil {
		return nil, err
	}
	return buildCapacityMonitorResponse(groupID, snapshot, history, horizon), nil
}

func buildCapacityMonitorResponse(groupID int64, snapshot *GroupCapacityMonitorSnapshot, history, horizon time.Duration) *CapacityMonitorResponse {
	now := snapshot.AsOf.UTC()
	r := &CapacityMonitorResponse{GroupID: groupID, GroupName: snapshot.GroupName, AsOf: now,
		Timezone: timezone.Name(), Currency: "USD", FeeBasis: "account_stats_cost_x_account_rate_multiplier", ModelVersion: CapacityMonitorModelVersion,
		Scope: "current_member_openai_oauth_global_text", MembershipAsOf: now, History: []CapacityMonitorHistory{},
		SharedUsage: snapshot.SharedUsage, Quality: CapacityMonitorQuality{HistoryCompleteness: "unknown", WindowProvenance: "approximate_shared_timestamp", IdentityQuality: "provider_identity_approximate", UnclassifiedCost: monitorRound(snapshot.UnclassifiedCost)},
		Limitations: []string{"account_cost_equivalent_usd_not_provider_balance", "shared_timestamp_cannot_prove_each_window_observed_at", "request_completion_time_approximates_quota_sampling_boundary", "history_completeness_unknown", "provider_identity_pair_is_approximate_deduplication", "external_usage_and_future_account_changes_unknown", "positive_cumulative_gap_does_not_guarantee_schedulability"}}
	r.Cache.ExpiresAt = now.Add(time.Minute)
	if r.SharedUsage == nil {
		r.SharedUsage = []CapacityMonitorSharedUsage{}
	}
	ids := make([]int64, 0, len(snapshot.Accounts))
	for _, a := range snapshot.Accounts {
		ids = append(ids, a.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	sum := sha256.Sum256([]byte(fmt.Sprint(ids)))
	r.MembershipFingerprint = hex.EncodeToString(sum[:8])
	end := now.Truncate(time.Hour)
	shared := map[string]*CapacityMonitorSharedUsage{}
	for _, entry := range snapshot.SharedUsage {
		if !entry.BucketStart.IsZero() && entry.BucketStart.Before(end.Add(-history)) {
			continue
		}
		key := "null"
		if entry.GroupID != nil {
			key = fmt.Sprint(*entry.GroupID)
		}
		if item := shared[key]; item != nil {
			item.Cost += entry.Cost
		} else {
			copy := entry
			shared[key] = &copy
		}
	}
	r.SharedUsage = []CapacityMonitorSharedUsage{}
	for _, entry := range shared {
		r.SharedUsage = append(r.SharedUsage, *entry)
	}
	for _, p := range snapshot.History {
		if !p.StartAt.Before(end.Add(-history)) {
			r.History = append(r.History, p)
		}
	}
	r.DemandBases = monitorDemandBases(snapshot.History, end)
	events := monitorCapacityEvents(snapshot.Accounts, now, 15*24*time.Hour, &r.Inventory, &r.Quality)
	markers := []time.Time{}
	for _, demand := range r.DemandBases {
		scenario := monitorScenario(events, now, horizon, r.Inventory.ModeledCount, demand)
		r.Scenarios = append(r.Scenarios, scenario)
		if scenario.FirstShortfallAt != nil {
			markers = append(markers, *scenario.FirstShortfallAt)
		}
	}
	r.CapacityPoints = monitorCapacityPoints(events, now, horizon, r.Inventory.ModeledCount, markers...)
	if len(events) > 2400 {
		r.Limitations = append(r.Limitations, "display_grid_five_minutes_exact_pressure")
	}
	for i := range r.History {
		p := &r.History[i]
		p.PoolAccountCost = monitorRound(p.PoolAccountCost)
		p.GroupRouteCost = monitorRound(p.GroupRouteCost)
		p.PoolViaGroupCost = monitorRound(p.PoolViaGroupCost)
		p.PoolOtherCost = monitorRound(p.PoolOtherCost)
	}
	for i := range r.SharedUsage {
		r.SharedUsage[i].Cost = monitorRound(r.SharedUsage[i].Cost)
	}
	sort.Slice(r.SharedUsage, func(i, j int) bool { return r.SharedUsage[i].Cost > r.SharedUsage[j].Cost })
	return r
}
