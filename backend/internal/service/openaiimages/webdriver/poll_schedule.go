package webdriver

import (
	"math/rand"
	"time"
)

// Post-SSE conversation poll strategy (anti-429 / anti-inaccessible).
//
// While SSE is open we never GET /conversation.
// After SSE ends without asset pointers we wait PollInitialWait (default 10s)
// before the first GET — live traffic shows offset=0 almost always returns
// conversation_inaccessible (document not committed yet; same lesson as chatgpt2api).
// Subsequent GETs use a fixed interval (default 10s) with jitter and hard
// cool-down after HTTP 429 so we do not hammer the account.
//
// Soft poll 429 must never durable-pin image quota (handled by the service layer).

const (
	defaultPollMinGap      = 10 * time.Second
	defaultPollInitialWait = 10 * time.Second
	poll429Base            = 15 * time.Second
	poll429Max             = 60 * time.Second
)

// pollScheduleOffsets returns absolute times (from poll start) for conversation GETs.
// initialWait: delay before first GET (chatgpt2api image_poll_initial_wait_secs).
// minGap: interval between subsequent attempts (image_poll_interval_secs).
func pollScheduleOffsets(timeout, minGap, initialWait time.Duration) []time.Duration {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	if minGap <= 0 {
		minGap = defaultPollMinGap
	}
	if initialWait < 0 {
		initialWait = 0
	}
	if initialWait >= timeout {
		// Still attempt once near the end of the budget.
		final := timeout - time.Second
		if final < 0 {
			final = 0
		}
		return []time.Duration{final}
	}

	out := make([]time.Duration, 0, int(timeout/minGap)+3)
	at := initialWait
	for at < timeout {
		out = append(out, at)
		next := at + minGap
		if next <= at {
			break
		}
		at = next
	}
	// Ensure a final attempt near timeout when the arithmetic grid ended early.
	if len(out) == 0 {
		final := timeout - time.Second
		if final < 0 {
			final = 0
		}
		return []time.Duration{final}
	}
	last := out[len(out)-1]
	final := timeout - time.Second
	if final < 0 {
		final = timeout
	}
	// Only append a near-timeout shot when it still respects minGap.
	if final > last && final-last >= minGap {
		out = append(out, final)
	}
	return out
}

// pollBackoffAfter429 is cool-down after conversation GET 429.
// Milder base than the previous 25s×2 so we can keep trying within timeout
// instead of aborting after three hits.
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

// waitUntilTarget sleeps until wall target or ctx/deadline, returns sleep duration.
func waitUntilTarget(now, target, deadline time.Time) time.Duration {
	if target.After(deadline) {
		target = deadline
	}
	if !target.After(now) {
		return 0
	}
	return target.Sub(now)
}
