package utils

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// TestDrainErrGroupReturnsError verifies the normal path: when the group's
// goroutines finish before the timeout, DrainErrGroup returns g.Wait()'s error
// verbatim (and does not wait out the timeout).
func TestDrainErrGroupReturnsError(t *testing.T) {
	var g errgroup.Group
	want := errors.New("boom")
	g.Go(func() error { return want })

	start := time.Now()
	got := DrainErrGroup(zap.NewNop(), "test", &g, time.Second)
	if !errors.Is(got, want) {
		t.Fatalf("DrainErrGroup err = %v, want %v", got, want)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("DrainErrGroup blocked for %s on a fast group; should return immediately", elapsed)
	}
}

// TestDrainErrGroupTimesOutOnHang is the regression guard for the shutdown
// deadlock: a goroutine that ignores cancellation (here, blocks forever) must
// NOT be able to make DrainErrGroup block past its timeout. Without the bound,
// the teardown that calls this would hang until an external SIGKILL.
func TestDrainErrGroupTimesOutOnHang(t *testing.T) {
	var g errgroup.Group
	block := make(chan struct{})
	g.Go(func() error {
		<-block // simulates a goroutine that never observes context cancellation
		return nil
	})

	start := time.Now()
	got := DrainErrGroup(zap.NewNop(), "test", &g, 50*time.Millisecond)
	elapsed := time.Since(start)

	if got != nil {
		t.Fatalf("DrainErrGroup err = %v, want nil on timeout", got)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("DrainErrGroup did not return promptly on a hung group: took %s (the deadlock would persist)", elapsed)
	}
	close(block) // unblock the leaked goroutine so the test process stays clean
}
