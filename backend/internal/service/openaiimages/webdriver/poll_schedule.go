package webdriver

import "time"

// Adaptive poll + 429 backoff for GET /backend-api/conversation/{id}.
//
// ONLY used AFTER SSE ends/disconnects/idles without asset pointers.
// While SSE is open we do not poll at all.
//
// After SSE, image assets (and free-plan limit text) often appear only in the
// conversation JSON — so the first ~20s of fallback poll must be reasonably
// snappy, then back off to protect the account from 429.

const (
	// Post-SSE catch-up (image often ready shortly after stream ends).
	pollPhaseFast   = 700 * time.Millisecond
	pollPhaseSteady = 1200 * time.Millisecond
	pollPhaseSlow   = 2 * time.Second
	pollPhaseIdle   = 2500 * time.Millisecond
	pollFastUntil   = 20 * time.Second
	pollSteadyUntil = 45 * time.Second
	pollSlowUntil   = 90 * time.Second
	poll429Base     = 2 * time.Second
	poll429Max      = 15 * time.Second
)

func pollIntervalForElapsed(elapsed, base time.Duration) time.Duration {
	var phase time.Duration
	switch {
	case elapsed < pollFastUntil:
		phase = pollPhaseFast
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
