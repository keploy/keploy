package consumerfake

import (
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer"
)

// Clock must satisfy the interface the gate and the recorder take, or the
// fake is testing something other than the production seam.
var _ consumer.Clock = (*Clock)(nil)

// Clock is a deterministic consumer.Clock for tests.
//
// Every interesting case in the completion rule is a timing case — the effect
// that lands one millisecond after the grace, the extra that lands during the
// drain, the backstop firing on a worker that never produced. Written with
// real sleeps those tests would be slow when they pass and flaky when they
// fail, and a flaky test here is indistinguishable from the product bug the
// rule exists to catch. On this clock they finish in microseconds and either
// always pass or always fail.
//
// Registration is DEADLINE-shaped, matching the consumer.Clock interface, so
// there is no read-then-register race: a waiter whose deadline has already
// passed at registration time fires immediately instead of being lost.
type Clock struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	waiters []*waiter
}

type waiter struct {
	deadline time.Time
	ch       chan time.Time
}

// NewClock returns a Clock reading start. A zero start is replaced with a
// fixed, arbitrary, non-zero instant: the zero time is what several
// "unpopulated" checks in this contract test for, so a clock that reads it
// would make those checks pass for the wrong reason.
func NewClock(start time.Time) *Clock {
	if start.IsZero() {
		start = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	}
	c := &Clock{now: start}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Now implements consumer.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Until implements consumer.Clock.
func (c *Clock) Until(deadline time.Time) (<-chan time.Time, func()) {
	c.mu.Lock()
	w := &waiter{deadline: deadline, ch: make(chan time.Time, 1)}
	if !c.now.Before(deadline) {
		w.ch <- c.now
		c.mu.Unlock()
		return w.ch, func() {}
	}
	c.waiters = append(c.waiters, w)
	c.cond.Broadcast()
	c.mu.Unlock()
	return w.ch, func() { c.stop(w) }
}

func (c *Clock) stop(target *waiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, w := range c.waiters {
		if w == target {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			c.cond.Broadcast()
			return
		}
	}
}

// Advance moves the clock forward by d and fires every waiter that is now due.
func (c *Clock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

// Set moves the clock to t (never backwards) and fires every waiter that is
// now due.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	if t.After(c.now) {
		c.now = t
	}
	now := c.now
	keep := c.waiters[:0]
	var due []*waiter
	for _, w := range c.waiters {
		if !now.Before(w.deadline) {
			due = append(due, w)
			continue
		}
		keep = append(keep, w)
	}
	c.waiters = keep
	c.cond.Broadcast()
	c.mu.Unlock()

	for _, w := range due {
		w.ch <- now
	}
}

// Deadlines lists the deadlines currently waited on.
func (c *Clock) Deadlines() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Time, 0, len(c.waiters))
	for _, w := range c.waiters {
		out = append(out, w.deadline)
	}
	return out
}

// WaitBlockedAt blocks until something is waiting on exactly this deadline.
//
// It is stricter than WaitBlocked on purpose. "Some timer is registered" is
// satisfied by a timer left over from a previous iteration — a completion loop
// parked on its far-away timeout backstop looks identical to one parked on the
// grace it just recomputed. A test that only asserts "not finished yet" after
// such a wait proves nothing, because the loop may simply not have woken. This
// waits for the SPECIFIC deadline the rule under test should have computed, so
// a rule that computes a different one fails here with a clear message instead
// of passing by luck.
func (c *Clock) WaitBlockedAt(deadline time.Time, budget time.Duration) bool {
	timer := time.AfterFunc(budget, func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer timer.Stop()

	until := time.Now().Add(budget)
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		for _, w := range c.waiters {
			if w.deadline.Equal(deadline) {
				return true
			}
		}
		if !time.Now().Before(until) {
			return false
		}
		c.cond.Wait()
	}
}

// Blocked reports how many waiters are currently registered.
func (c *Clock) Blocked() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// WaitBlocked blocks until at least n waiters are registered, returning false
// if that has not happened within the wall-clock budget.
//
// The budget is a FAILURE backstop, not a synchronisation delay: on a passing
// run this returns as soon as the goroutine under test registers its timer. It
// exists so a test that asserts a negative ("nothing has completed yet") can
// first make sure the code under test has actually reached its wait, instead
// of racing it.
func (c *Clock) WaitBlocked(n int, budget time.Duration) bool {
	timer := time.AfterFunc(budget, func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer timer.Stop()

	deadline := time.Now().Add(budget)
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.waiters) < n {
		if !time.Now().Before(deadline) {
			return len(c.waiters) >= n
		}
		c.cond.Wait()
	}
	return true
}
