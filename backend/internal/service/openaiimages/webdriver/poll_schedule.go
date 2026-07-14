package webdriver

import "time"

// Adaptive poll + 429 backoff for GET /backend-api/conversation/{id}.
//
// ONLY used AFTER SSE ends/disconnects/idles without asset pointers.
// While SSE is open we do not poll at all.
//
// Design goals (anti-429):
//   - Prefer SSE for early assets; poll is a fallback only.
//   - Steady cadence ~4s (not sub-second) so browser / multi-instance share quota.
//   - First GET in pollImages() is immediate (no pre-wait) — image is often already
//     in conversation JSON right after SSE ends.
//   - Slow further on long waits; exponential backoff on 429.

const (
	// Steady post-SSE poll: ~15 req/min max per conversation, much safer than 700ms.
	pollPhaseSteady = 4 * time.Second
	// After 2 minutes still waiting, ease further.
	pollPhaseSlow = 6 * time.Second
	// Near overall timeout, keep gentle pressure without hammering.
	pollPhaseIdle = 8 * time.Second

	pollSteadyUntil = 120 * time.Second
	pollSlowUntil   = 150 * time.Second

	// 429 backoff: start above steady interval so we actually cool down.
	poll429Base = 8 * time.Second
	poll429Max  = 30 * time.Second
)

func pollIntervalForElapsed(elapsed, base time.Duration) time.Duration {
	var phase time.Duration
	switch {
	case elapsed < pollSteadyUntil:
		phase = pollPhaseSteady
	case elapsed < pollSlowUntil:
		phase = pollPhaseSlow
	default:
		phase = pollPhaseIdle
	}
	if base > phase {
		return base
	}
	return phase
}

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
