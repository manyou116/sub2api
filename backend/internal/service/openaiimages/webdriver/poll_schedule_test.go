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
		{0, pollPhaseFast},
		{10 * time.Second, pollPhaseFast},
		{25 * time.Second, pollPhaseSteady},
		{60 * time.Second, pollPhaseSlow},
		{100 * time.Second, pollPhaseIdle},
	}
	for _, tc := range cases {
		got := pollIntervalForElapsed(tc.elapsed, 0)
		if got != tc.want {
			t.Fatalf("elapsed=%v got=%v want=%v", tc.elapsed, got, tc.want)
		}
	}
	if got := pollIntervalForElapsed(0, 2*time.Second); got != 2*time.Second {
		t.Fatalf("base floor: %v", got)
	}
}

func TestPollBackoffAfter429(t *testing.T) {
	if got := pollBackoffAfter429(1); got != 2*time.Second {
		t.Fatalf("1: %v", got)
	}
	if got := pollBackoffAfter429(2); got != 4*time.Second {
		t.Fatalf("2: %v", got)
	}
	if got := pollBackoffAfter429(3); got != 8*time.Second {
		t.Fatalf("3: %v", got)
	}
	if got := pollBackoffAfter429(4); got != 15*time.Second {
		t.Fatalf("4: %v", got)
	}
	if got := pollBackoffAfter429(99); got != 15*time.Second {
		t.Fatalf("cap: %v", got)
	}
}
