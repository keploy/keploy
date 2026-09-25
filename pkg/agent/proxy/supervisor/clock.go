package supervisor

import "time"

// clock is the hang watchdog's source of time. A reading is the time
// elapsed on the monotonic clock since the clock was made, so a step
// of the wall clock — an NTP correction, a VM resuming and resetting
// its date, an operator changing it — can neither declare a live
// parser hung nor keep a hung one from being caught.
//
// Tests substitute a fake that only moves when told to, so what the
// watchdog decides never depends on how goroutines were scheduled.
type clock interface {
	// now returns the current reading.
	now() time.Duration
	// newTicker returns a channel that paces the watchdog's checks,
	// one every d, and a func that releases it.
	newTicker(d time.Duration) (<-chan time.Time, func())
}

// monoClock is the clock every Supervisor made by New uses.
type monoClock struct {
	// origin carries a monotonic reading, which makes time.Since
	// subtract monotonic readings and ignore the wall clock.
	origin time.Time
}

func newMonoClock() monoClock { return monoClock{origin: time.Now()} }

func (c monoClock) now() time.Duration { return time.Since(c.origin) }

func (monoClock) newTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}
