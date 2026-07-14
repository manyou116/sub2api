package webdriver

import (
	"math/rand"
	"time"
)

// Post-SSE conversation poll strategy (anti-429).
//
// While SSE is open we never GET /conversation.
// After SSE ends without asset pointers we use a SPARSE absolute schedule:
// few GETs over ~3 minutes instead of a fixed 4s ticker (~45 GETs).
//
// On 429 we do NOT keep the same cadence — we jump forward with a hard cool-down
// so the account can recover; continuous retry-after-429 is what made images
// "never appear" (upstream keeps refusing while we keep hammering).

const (
	poll429Base = 25 * time.Second
	poll429Max  = 90 * time.Second
)

// Default absolute offsets from poll start (first GET is immediate at 0).
// ~11 attempts max over 3 minutes vs ~45 at 4s interval.
var defaultPollOffsets = []time.Duration{
	0,
	5 * time.Second,
	12 * time.Second,
	22 * time.Second,
	35 * time.Second,
	50 * time.Second,
	70 * time.Second,
	95 * time.Second,
	120 * time.Second,
	150 * time.Second,
	180 * time.Second,
}

// pollScheduleOffsets returns absolute times to attempt GET, clipped to timeout.
// minGap is a floor between attempts (from Driver.PollInterval); default schedule
// already spaces wider than 4s after the first two points.
func pollScheduleOffsets(timeout, minGap time.Duration) []time.Duration {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	if minGap < 0 {
		minGap = 0
	}
	out := make([]time.Duration, 0, len(defaultPollOffsets)+4)
	var last time.Duration = -1
	for _, at := range defaultPollOffsets {
		if at > timeout {
			break
		}
		if last >= 0 && minGap > 0 && at-last < minGap {
			at = last + minGap
			if at > timeout {
				break
			}
		}
		out = append(out, at)
		last = at
	}
	// Ensure a final attempt near timeout if schedule ended early.
	if len(out) == 0 || out[len(out)-1] < timeout {
		final := timeout - time.Second
		if final < 0 {
			final = timeout
		}
		if last < 0 || final-last >= minGap || minGap == 0 {
			if final > last {
				out = append(out, final)
			}
		}
	}
	return out
}

// pollBackoffAfter429 is hard cool-down after conversation GET 429.
// Starts above typical image finish latency so we stop feeding the rate limiter.
func pollBackoffAfter429(consecutive int) time.Duration {
	if consecutive < 1 {
		consecutive = 1
	}
	d := poll429Base
	for i := 1; i < consecutive; i++ {
		d *= 2
		if d >= poll429Max {
			return poll429Max
		}
	}
	if d > poll429Max {
		return poll429Max
	}
	return d
}

// jitterDuration applies ±20% jitter to avoid multi-instance lockstep.
func jitterDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	// 0.8 .. 1.2
	f := 0.8 + rand.Float64()*0.4
	return time.Duration(float64(d) * f)
}

// waitUntilTarget sleeps until wall target or ctx/deadline, returns false if cancelled.
func waitUntilTarget(now, target, deadline time.Time) time.Duration {
	if target.After(deadline) {
		target = deadline
	}
	if !target.After(now) {
		return 0
	}
	return target.Sub(now)
}
