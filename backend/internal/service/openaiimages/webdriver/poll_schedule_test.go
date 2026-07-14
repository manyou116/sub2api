package webdriver

import (
	"testing"
	"time"
)

func TestPollScheduleOffsetsSparse(t *testing.T) {
	offs := pollScheduleOffsets(180*time.Second, 0)
	if len(offs) < 8 || len(offs) > 14 {
		t.Fatalf("unexpected count %d: %v", len(offs), offs)
	}
	if offs[0] != 0 {
		t.Fatalf("first must be immediate, got %v", offs[0])
	}
	// No sub-second hammering in the schedule itself.
	for i := 1; i < len(offs); i++ {
		gap := offs[i] - offs[i-1]
		if gap < 4*time.Second {
			t.Fatalf("gap[%d]=%v too small (%v)", i, gap, offs)
		}
	}
}

func TestPollScheduleOffsetsRespectsMinGap(t *testing.T) {
	offs := pollScheduleOffsets(60*time.Second, 10*time.Second)
	for i := 1; i < len(offs); i++ {
		if offs[i]-offs[i-1] < 10*time.Second {
			t.Fatalf("minGap violated: %v", offs)
		}
	}
}

func TestPollBackoffAfter429(t *testing.T) {
	if got := pollBackoffAfter429(1); got != 25*time.Second {
		t.Fatalf("1: %v", got)
	}
	if got := pollBackoffAfter429(2); got != 50*time.Second {
		t.Fatalf("2: %v", got)
	}
	if got := pollBackoffAfter429(3); got != 90*time.Second {
		t.Fatalf("3: %v", got)
	}
	if got := pollBackoffAfter429(99); got != 90*time.Second {
		t.Fatalf("cap: %v", got)
	}
}

func TestWaitUntilTarget(t *testing.T) {
	now := time.Unix(1000, 0)
	deadline := now.Add(60 * time.Second)
	if d := waitUntilTarget(now, now.Add(5*time.Second), deadline); d != 5*time.Second {
		t.Fatalf("got %v", d)
	}
	if d := waitUntilTarget(now, now.Add(-time.Second), deadline); d != 0 {
		t.Fatalf("past target: %v", d)
	}
}
