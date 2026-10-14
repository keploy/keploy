package utils

import (
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// A group still making progress is waited for past the stall bound; the bound
// is on progress, not on the total.
func TestDrainErrGroupProgressWaitsWhileProgressing(t *testing.T) {
	var g errgroup.Group
	var p atomic.Uint64
	const stall = 100 * time.Millisecond
	g.Go(func() error {
		for i := 0; i < 20; i++ { // 20 x 25 ms: 5 stall windows in all
			time.Sleep(25 * time.Millisecond)
			p.Add(1)
		}
		return nil
	})
	start := time.Now()
	err, timedOut := DrainErrGroupProgress(zap.NewNop(), "t", &g, stall, p.Load)
	if err != nil || timedOut {
		t.Fatalf("(%v, timedOut=%v) after %v: a draining group was cut off", err, timedOut, time.Since(start))
	}
}

// A group that stops making progress is given up on after the stall bound.
func TestDrainErrGroupProgressGivesUpOnAStall(t *testing.T) {
	var g errgroup.Group
	var p atomic.Uint64
	block := make(chan struct{})
	defer close(block)
	g.Go(func() error { <-block; return nil })
	start := time.Now()
	_, timedOut := DrainErrGroupProgress(zap.NewNop(), "t", &g, 100*time.Millisecond, p.Load)
	if took := time.Since(start); !timedOut || took > 2*time.Second {
		t.Fatalf("timedOut=%v after %v, want a give-up about 100ms after the last progress", timedOut, took)
	}
}
