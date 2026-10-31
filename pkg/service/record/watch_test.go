package record

import (
	"testing"
	"time"
)

func TestWatchBackoff(t *testing.T) {
	var b watchBackoff
	start := time.Unix(0, 0)

	// A crash loop doubles the delay up to the cap.
	now := start
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := b.next(now); got != w {
			t.Fatalf("restart %d: delay = %v, want %v", i, got, w)
		}
		now = now.Add(b.delay + time.Second)
	}

	// An app that stayed up past the stable window starts over at the minimum.
	now = now.Add(watchBackoffMax + watchBackoffStable)
	if got := b.next(now); got != watchBackoffMin {
		t.Fatalf("after a stable run: delay = %v, want %v", got, watchBackoffMin)
	}
}
