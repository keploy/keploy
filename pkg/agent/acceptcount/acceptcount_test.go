package acceptcount

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// waiting returns once a WaitAccepted is parked on the counter: it has
// looked, found too few, and waits for the next change.
func waiting(c *Counter) {
	for {
		c.mu.Lock()
		parked := c.changed != nil
		c.mu.Unlock()
		if parked {
			return
		}
		runtime.Gosched()
	}
}

// Accepted is the highest n with every number 1..n accepted: a number
// accepted ahead of a lower one counts only once the lower one is in.
func TestAcceptedIsTheUnbrokenRunFromOne(t *testing.T) {
	var c Counter
	if n, err := c.Accepted(); n != 0 || err != nil {
		t.Fatalf("a fresh counter: %d, %v; want 0", n, err)
	}
	for _, step := range []struct{ seq, want uint64 }{
		{1, 1}, {3, 1}, {4, 1}, {2, 4}, {6, 4}, {5, 6},
	} {
		c.Add(step.seq)
		if n, err := c.Accepted(); n != step.want || err != nil {
			t.Fatalf("after %d: Accepted = %d, %v; want %d", step.seq, n, err, step.want)
		}
	}
}

// A wait for n returns once 1..n are all accepted, however many later
// numbers arrive first: later connections cannot hold it back, and one
// accepted ahead of an earlier one cannot end it early.
func TestWaitAcceptedWaitsForEveryNumberUpToN(t *testing.T) {
	var c Counter
	if err := c.WaitAccepted(context.Background(), 0); err != nil {
		t.Fatalf("waiting for 0: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.WaitAccepted(context.Background(), 2) }()
	waiting(&c)
	c.Add(1)
	waiting(&c) // woken at 1, parked again: 2 is still out
	for seq := uint64(3); seq < 1000; seq++ {
		c.Add(seq) // a steady stream of later connections, all ahead of 2
	}
	select {
	case err := <-done:
		t.Fatalf("WaitAccepted(2) returned %v with 2 not accepted", err)
	default:
	}
	c.Add(2)
	if err := <-done; err != nil {
		t.Fatalf("WaitAccepted: %v", err)
	}
	if n, _ := c.Accepted(); n != 999 {
		t.Fatalf("Accepted = %d, want 999", n)
	}
}

func TestWaitAcceptedEndsWithItsContext(t *testing.T) {
	var c Counter
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.WaitAccepted(ctx, 1) }()
	waiting(&c)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitAccepted after cancel: %v, want context.Canceled", err)
	}
}

// A listener that closes ends every wait with its reason, then and after.
func TestFailEndsEveryWait(t *testing.T) {
	var c Counter
	c.Add(1)
	closed := errors.New("the listener closed")
	done := make(chan error, 1)
	go func() { done <- c.WaitAccepted(context.Background(), 2) }()
	waiting(&c)
	c.Fail(closed)
	if err := <-done; !errors.Is(err, closed) {
		t.Fatalf("a parked WaitAccepted after Fail: %v, want its error", err)
	}
	if err := c.WaitAccepted(context.Background(), 1); !errors.Is(err, closed) {
		t.Fatalf("a WaitAccepted after Fail, its count reached: %v, want Fail's error", err)
	}
	c.Fail(errors.New("later"))
	if n, err := c.Accepted(); n != 1 || !errors.Is(err, closed) {
		t.Fatalf("Accepted after Fail: %d, %v; want 1 and the first error", n, err)
	}
}

// A number the kernel never gives (0), or one given twice, is a numbering
// keploy cannot trust: the counter fails, never miscounts.
func TestABrokenNumberingFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		seqs []uint64
		want string
	}{
		{"no number", []uint64{1, 0}, "no sequence number"},
		{"a number twice, below the run", []uint64{1, 2, 1}, "accepted twice"},
		{"a number twice, ahead of the run", []uint64{3, 3}, "accepted twice"},
	} {
		var c Counter
		for _, s := range tc.seqs {
			c.Add(s)
		}
		if _, err := c.Accepted(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v, want an error saying %q", tc.name, err, tc.want)
		}
	}
}
