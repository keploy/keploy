package models

import (
	"testing"
	"time"
)

func TestThrottleDurationDefault(t *testing.T) {
	if got := (AsyncLane{}).ThrottleDuration(); got != time.Second {
		t.Fatalf("unset throttle: want 1s, got %v", got)
	}
	if got := (AsyncLane{ThrottleMs: 0}).ThrottleDuration(); got != time.Second {
		t.Fatalf("zero throttle: want 1s, got %v", got)
	}
}

func TestThrottleDurationExplicit(t *testing.T) {
	if got := (AsyncLane{ThrottleMs: 250}).ThrottleDuration(); got != 250*time.Millisecond {
		t.Fatalf("want 250ms, got %v", got)
	}
}
