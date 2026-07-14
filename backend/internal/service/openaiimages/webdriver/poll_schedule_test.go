package webdriver

import (
	"testing"
	"time"
)

func TestPollIntervalForElapsed(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    time.Duration
	}{
		{0, pollPhaseSteady},
		{30 * time.Second, pollPhaseSteady},
		{119 * time.Second, pollPhaseSteady},
		{120 * time.Second, pollPhaseSlow},
		{140 * time.Second, pollPhaseSlow},
		{150 * time.Second, pollPhaseIdle},
		{200 * time.Second, pollPhaseIdle},
	}
	for _, tc := range cases {
		got := pollIntervalForElapsed(tc.elapsed, 0)
		if got != tc.want {
			t.Fatalf("elapsed=%v got=%v want=%v", tc.elapsed, got, tc.want)
		}
	}
	// Configured PollInterval acts as a floor (never faster than base).
	if got := pollIntervalForElapsed(0, 5*time.Second); got != 5*time.Second {
		t.Fatalf("base floor: %v", got)
	}
	// Base below phase does not force faster polling.
	if got := pollIntervalForElapsed(0, time.Second); got != pollPhaseSteady {
		t.Fatalf("ignore low base: %v", got)
	}
}

func TestPollBackoffAfter429(t *testing.T) {
	if got := pollBackoffAfter429(1); got != 8*time.Second {
		t.Fatalf("1: %v", got)
	}
	if got := pollBackoffAfter429(2); got != 16*time.Second {
		t.Fatalf("2: %v", got)
	}
	if got := pollBackoffAfter429(3); got != 30*time.Second {
		t.Fatalf("3: %v", got)
	}
	if got := pollBackoffAfter429(99); got != 30*time.Second {
		t.Fatalf("cap: %v", got)
	}
}
