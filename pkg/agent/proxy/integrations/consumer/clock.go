package consumer

import "time"

// Clock is the only source of time in this package.
//
// WHY AN INTERFACE AND NOT time.Now(). The completion rule (§5) is
// "expected count observed AND a grace window elapsed, with a timeout
// backstop". Every interesting case in that rule is a timing case: the effect
// that lands one millisecond after the grace, the extra that lands during the
// drain, the backstop firing on a worker that never produced. A test that
// exercises those with real sleeps is a test that is slow when it passes and
// flaky when it fails — and a flaky test here is indistinguishable from the
// product bug the rule exists to catch. Every timing case in this package's
// suite therefore runs on a fake clock and finishes in microseconds.
//
// Until IS DELIBERATELY DEADLINE-SHAPED, NOT DURATION-SHAPED. A duration-based
// timer ("fire in d") has an unavoidable read-then-register race against a
// clock a test can advance: the caller reads Now, computes d, and the clock
// can move between those two steps, so the timer is registered d too late and
// the wake-up is lost. Passing the absolute deadline removes the window: the
// implementation compares the deadline against the clock at registration time
// and fires immediately when it has already passed.
type Clock interface {
	// Now returns the current time.
	Now() time.Time

	// Until returns a channel that receives once the clock reaches
	// deadline — immediately if it already has — together with a stop
	// function that releases the timer. The stop function must be safe to
	// call whether or not the channel already fired, and safe to call more
	// than once.
	Until(deadline time.Time) (<-chan time.Time, func())
}

// realClock is the production Clock: wall time and real timers.
type realClock struct{}

// RealClock returns the production Clock. It is the default for a Gate or a
// Recorder constructed with a nil clock, so no production call site has to
// name it.
func RealClock() Clock { return realClock{} }

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Until(deadline time.Time) (<-chan time.Time, func()) {
	d := time.Until(deadline)
	if d < 0 {
		// time.NewTimer rejects nothing here, but a negative duration
		// would still schedule at once; normalising to zero makes the
		// "already past" case explicit rather than incidental.
		d = 0
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// clockOrReal normalises a possibly-nil Clock to the production one.
func clockOrReal(c Clock) Clock {
	if c == nil {
		return RealClock()
	}
	return c
}
