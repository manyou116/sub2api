package service

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

type monitorWindow struct {
	total, remaining float64
	reset            time.Time
}
type monitorEvent struct {
	at                time.Time
	direct, estimated float64
}

func monitorRound(v float64) float64 { return math.Round(v*1e6) / 1e6 }
func monitorNumber(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	v = monitorRound(v)
	return &v
}

func monitorTime(extra map[string]any, key string) (time.Time, bool) {
	v, ok := extra[key]
	if !ok {
		return time.Time{}, false
	}
	t, err := parseTime(fmt.Sprint(v))
	return t.UTC(), err == nil && !t.IsZero()
}

// CapacityMonitorSampleBounds returns approximate shared timestamp boundaries.
// Existing extra fields cannot prove both windows came from that same sample.
func CapacityMonitorSampleBounds(a CapacityMonitorAccount) (observed, start5, start7 time.Time, ok bool) {
	observed, valid := monitorTime(a.Extra, "codex_usage_updated_at")
	if !valid {
		return
	}
	r5, valid5 := monitorTime(a.Extra, "codex_5h_reset_at")
	r7, valid7 := monitorTime(a.Extra, "codex_7d_reset_at")
	if !valid5 || !valid7 || parseExtraFloat64(a.Extra["codex_5h_window_minutes"]) != 300 || parseExtraFloat64(a.Extra["codex_7d_window_minutes"]) != 10080 {
		return
	}
	start5, start7 = r5.Add(-5*time.Hour), r7.Add(-7*24*time.Hour)
	ok = !observed.Before(start5) && !observed.Before(start7) && observed.Before(r5) && observed.Before(r7)
	return
}

func monitorEligible(a CapacityMonitorAccount) bool {
	return a.Platform == PlatformOpenAI && a.Type == AccountTypeOAuth && a.ParentID == nil && (a.QuotaDimension == "" || a.QuotaDimension == "global")
}

func monitorWindowEstimate(a CapacityMonitorAccount, key string, cost float64) float64 {
	u, ok := monitorPercent(a.Extra, "codex_"+key+"_used_percent")
	if !ok {
		return 0
	}
	if u < 1 || u > 100 || cost <= 0 || math.IsNaN(u) || math.IsInf(u, 0) {
		return 0
	}
	v := cost * 100 / u
	if math.IsNaN(v) || math.IsInf(v, 0) || v > 1e8 {
		return 0
	}
	return v
}

func monitorPercent(extra map[string]any, key string) (float64, bool) {
	raw, ok := extra[key]
	if !ok || raw == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(fmt.Sprint(raw), 64)
	return v, err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 100
}

func monitorMedian(samples []float64) float64 {
	if len(samples) < 3 {
		return 0
	}
	sort.Float64s(samples)
	n := len(samples)
	if n%2 == 0 {
		return (samples[n/2-1] + samples[n/2]) / 2
	}
	return samples[n/2]
}

func monitorCapacityEvents(accounts []CapacityMonitorAccount, now time.Time, horizon time.Duration, inventory *CapacityMonitorInventory, quality *CapacityMonitorQuality) []monitorEvent {
	inventory.MemberCount = len(accounts)
	// Select a single snapshot for a provider identity; unknown identities remain
	// individual rows and are explicitly counted, never claimed as physical dedup.
	unique := map[string]CapacityMonitorAccount{}
	duplicates := map[string]bool{}
	for _, a := range accounts {
		if !monitorEligible(a) {
			continue
		}
		inventory.EligibleCount++
		inventory.HistoryAccountCount++
		if a.Status != StatusActive || !a.Schedulable || (a.AutoPauseOnExpired && a.ExpiresAt != nil && !a.ExpiresAt.After(now)) {
			inventory.StateExcludedCount++
		} else {
			paused := false
			for _, until := range []*time.Time{a.RateResetAt, a.OverloadUntil, a.TempUntil} {
				if until != nil && until.After(now) {
					paused = true
				}
			}
			if !paused {
				inventory.SchedulableCount++
			}
		}
		if a.Shared {
			inventory.SharedCount++
		}
		key := a.IdentityKey
		if key == "" {
			key = fmt.Sprintf("row:%d", a.ID)
			inventory.UnknownIdentityCount++
		}
		if a.DuplicateIdentity {
			duplicates[key] = true
			inventory.DuplicateCount++
		}
		if old, ok := unique[key]; ok {
			if !a.DuplicateIdentity && !old.DuplicateIdentity {
				inventory.DuplicateCount++
			}
			duplicates[key] = true
			oldTime, _ := monitorTime(old.Extra, "codex_usage_updated_at")
			newTime, _ := monitorTime(a.Extra, "codex_usage_updated_at")
			if !newTime.After(oldTime) {
				continue
			}
		}
		unique[key] = a
	}
	if inventory.UnknownIdentityCount > 0 {
		quality.IdentityQuality = "partial"
	}
	if inventory.UnknownIdentityCount == inventory.EligibleCount && inventory.EligibleCount > 0 {
		quality.IdentityQuality = "unknown"
	}
	keys := make([]string, 0, len(unique))
	for k := range unique {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	samples5, samples7 := map[string][]float64{}, map[string][]float64{}
	valid := make([]CapacityMonitorAccount, 0, len(unique))
	for _, k := range keys {
		// Per-row accounting cannot reconstruct a duplicated provider identity's
		// aligned numerator, so exclude that entity rather than overstate supply.
		if duplicates[k] {
			continue
		}
		a := unique[k]
		if a.Status != StatusActive || !a.Schedulable || (a.AutoPauseOnExpired && a.ExpiresAt != nil && !a.ExpiresAt.After(now)) {
			continue
		}
		if a.TempUntil != nil && a.TempUntil.After(now) && !IsOpenAICapacityCooldownReason(a.TempReason) {
			continue
		}
		if a.IdentityKey == "" {
			continue
		}
		observed, _, _, ok := CapacityMonitorSampleBounds(a)
		_, ok5 := monitorPercent(a.Extra, "codex_5h_used_percent")
		_, ok7 := monitorPercent(a.Extra, "codex_7d_used_percent")
		if !ok || !ok5 || !ok7 {
			inventory.MissingWindowCount++
			continue
		}
		inventory.ProvenanceUnknownCount++
		if quality.FreshestSampleAt == nil || observed.After(*quality.FreshestSampleAt) {
			t := observed
			quality.FreshestSampleAt = &t
		}
		if quality.OldestSampleAt == nil || observed.Before(*quality.OldestSampleAt) {
			t := observed
			quality.OldestSampleAt = &t
		}
		if observed.After(now) || now.Sub(observed) > 2*time.Hour {
			inventory.StaleCount++
			continue
		}
		valid = append(valid, a)
		if a.Plan == "" {
			continue
		}
		if total := monitorWindowEstimate(a, "5h", a.Window5Cost); total > 0 {
			samples5[a.Plan] = append(samples5[a.Plan], total)
		}
		if total := monitorWindowEstimate(a, "7d", a.Window7Cost); total > 0 {
			samples7[a.Plan] = append(samples7[a.Plan], total)
		}
	}
	events := []monitorEvent{}
	for _, a := range valid {
		total5, total7 := monitorWindowEstimate(a, "5h", a.Window5Cost), monitorWindowEstimate(a, "7d", a.Window7Cost)
		estimated := false
		if total5 <= 0 && a.Plan != "" {
			total5 = monitorMedian(samples5[a.Plan])
			estimated = true
		}
		if total7 <= 0 && a.Plan != "" {
			total7 = monitorMedian(samples7[a.Plan])
			estimated = true
		}
		if total5 <= 0 || total7 <= 0 {
			continue
		}
		u5, _ := monitorPercent(a.Extra, "codex_5h_used_percent")
		u7, _ := monitorPercent(a.Extra, "codex_7d_used_percent")
		if u5 < 0 || u5 > 100 || u7 < 0 || u7 > 100 || math.IsNaN(u5) || math.IsNaN(u7) {
			continue
		}
		r5, _ := monitorTime(a.Extra, "codex_5h_reset_at")
		r7, _ := monitorTime(a.Extra, "codex_7d_reset_at")
		w5 := monitorWindow{total: total5, remaining: math.Max(0, total5*(1-u5/100)-a.Post5Cost), reset: r5}
		w7 := monitorWindow{total: total7, remaining: math.Max(0, total7*(1-u7/100)-a.Post7Cost), reset: r7}
		if !r5.After(now) {
			w5.remaining = math.Max(0, total5-a.Post5Cost)
			w5.reset = time.Time{}
			estimated = true
		}
		if !r7.After(now) {
			w7.remaining = math.Max(0, total7-a.Post7Cost)
			w7.reset = time.Time{}
			estimated = true
		}
		inventory.ModeledCount++
		if estimated {
			inventory.EstimatedCount++
		} else {
			inventory.DirectCount++
		}
		start := now
		for _, until := range []*time.Time{a.RateResetAt, a.OverloadUntil, a.TempUntil} {
			if until != nil && until.After(start) {
				start = *until
			}
		}
		stop := now.Add(horizon)
		if a.AutoPauseOnExpired && a.ExpiresAt != nil && a.ExpiresAt.Before(stop) {
			stop = *a.ExpiresAt
		}
		for at, iterations := start, 0; at.Before(stop) && iterations < 200; iterations++ {
			for _, w := range []*monitorWindow{&w5, &w7} {
				if !w.reset.IsZero() && !at.Before(w.reset) {
					w.remaining = w.total
					w.reset = time.Time{}
				}
			}
			amount := math.Min(w5.remaining, w7.remaining)
			if amount > 1e-9 {
				if w5.reset.IsZero() {
					w5.reset = at.Add(5 * time.Hour)
				}
				if w7.reset.IsZero() {
					w7.reset = at.Add(7 * 24 * time.Hour)
				}
				w5.remaining -= amount
				w7.remaining -= amount
				e := monitorEvent{at: at}
				if estimated {
					e.estimated = amount
				} else {
					e.direct = amount
				}
				events = append(events, e)
			}
			next := stop
			for _, w := range []*monitorWindow{&w5, &w7} {
				if w.reset.After(at) && w.reset.Before(next) {
					next = w.reset
				}
			}
			if !next.After(at) {
				break
			}
			at = next
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	merged := []monitorEvent{}
	for _, e := range events {
		n := len(merged)
		if n > 0 && merged[n-1].at.Equal(e.at) {
			merged[n-1].direct += e.direct
			merged[n-1].estimated += e.estimated
		} else {
			merged = append(merged, e)
		}
	}
	return merged
}

func monitorCapacityAt(events []monitorEvent, at, now time.Time) (direct, estimated float64) {
	for _, e := range events {
		if e.at.After(at) || (e.at.Equal(at) && !at.Equal(now)) {
			break
		}
		direct += e.direct
		estimated += e.estimated
	}
	return
}

func monitorCapacityPoints(events []monitorEvent, now time.Time, horizon time.Duration, count int, markers ...time.Time) []CapacityMonitorPoint {
	points := []CapacityMonitorPoint{}
	add := func(at time.Time, direct, estimated float64) {
		p := CapacityMonitorPoint{At: at, Hours: at.Sub(now).Hours(), AccountCount: count}
		if count > 0 {
			p.Capacity = monitorNumber(direct + estimated)
			p.DirectCapacity = monitorNumber(direct)
			p.EstimatedCapacity = monitorNumber(estimated)
		}
		points = append(points, p)
	}
	end := now.Add(horizon)
	times := []time.Time{now, end}
	for _, at := range markers {
		if at.After(now) && at.Before(end) {
			times = append(times, at)
		}
	}
	// Small pools preserve both sides of every exact release event. Larger pools
	// use a bounded five-minute display grid; pressure is always computed exactly.
	if len(events) <= 2400 {
		for _, event := range events {
			if event.at.After(now) && event.at.Before(end) {
				times = append(times, event.at, event.at.Add(time.Nanosecond))
			}
		}
	} else {
		for at := now.Add(5 * time.Minute); at.Before(end); at = at.Add(5 * time.Minute) {
			times = append(times, at)
		}
	}
	for _, h := range []int{1, 5, 24, 72, 168, 360} {
		at := now.Add(time.Duration(h) * time.Hour)
		if at.Before(end) {
			times = append(times, at)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	d, e, index := 0., 0., 0
	for i, at := range times {
		if i > 0 && at.Equal(times[i-1]) {
			continue
		}
		for index < len(events) && (events[index].at.Before(at) || (at.Equal(now) && events[index].at.Equal(now))) {
			d += events[index].direct
			e += events[index].estimated
			index++
		}
		add(at, d, e)
	}
	return points
}

func monitorDemandBases(history []CapacityMonitorHistory, end time.Time) []CapacityMonitorDemand {
	ranges := []struct {
		key        string
		start, end time.Time
	}{{"last24h", end.Add(-24 * time.Hour), end}, {"last7d", end.Add(-7 * 24 * time.Hour), end}, {"previous24h", end.Add(-48 * time.Hour), end.Add(-24 * time.Hour)}}
	result := []CapacityMonitorDemand{}
	for _, r := range ranges {
		d := CapacityMonitorDemand{Key: r.key, Hours: r.end.Sub(r.start).Hours(), Reason: "history_completeness_unknown"}
		buckets := 0
		requests := int64(0)
		for _, p := range history {
			if p.Complete && !p.StartAt.Before(r.start) && p.EndAt.Before(r.end.Add(time.Nanosecond)) {
				d.Cost += p.PoolAccountCost
				buckets++
				requests += p.PoolRequests
			}
		}
		d.Cost = monitorRound(d.Cost)
		if buckets == int(d.Hours) && requests > 0 && d.Cost > 0 {
			d.Available = true
			d.RatePerHour = monitorNumber(d.Cost / d.Hours)
		} else {
			d.Reason = "insufficient_recorded_history"
		}
		result = append(result, d)
	}
	return result
}

func monitorScenario(events []monitorEvent, now time.Time, horizon time.Duration, count int, demand CapacityMonitorDemand) CapacityMonitorScenario {
	s := CapacityMonitorScenario{Key: demand.Key, Milestones: []CapacityMonitorMilestone{}}
	for _, h := range []float64{1, 5, 24, 72, 168, 360} {
		m := CapacityMonitorMilestone{Hours: h}
		if count > 0 {
			d, e := monitorCapacityAt(events, now.Add(time.Duration(h*float64(time.Hour))), now)
			m.Capacity = monitorNumber(d + e)
		}
		if demand.Available && demand.RatePerHour != nil {
			m.Demand = monitorNumber(h * *demand.RatePerHour)
			if m.Capacity != nil {
				m.Gap = monitorNumber(*m.Capacity - *m.Demand)
			}
		}
		s.Milestones = append(s.Milestones, m)
	}
	if count == 0 || demand.RatePerHour == nil || *demand.RatePerHour <= 0 {
		return s
	}
	rate := *demand.RatePerHour
	end := now.Add(horizon)
	d, e := monitorCapacityAt(events, now, now)
	cumulative := d + e
	minimum := math.Inf(1)
	for i := 0; i <= len(events); i++ {
		if i < len(events) && !events[i].at.After(now) {
			continue
		}
		at := end
		if i < len(events) {
			at = events[i].at
		}
		if at.After(end) {
			at = end
		}
		gap := cumulative - at.Sub(now).Hours()*rate
		minimum = math.Min(minimum, gap)
		if s.FirstShortfallAt == nil && gap < -1e-9 {
			crossing := now.Add(time.Duration(cumulative / rate * float64(time.Hour)))
			s.FirstShortfallAt = &crossing
		}
		if i < len(events) && !events[i].at.After(end) {
			cumulative += events[i].direct + events[i].estimated
		}
	}
	s.MinimumGap = monitorNumber(minimum)
	return s
}
