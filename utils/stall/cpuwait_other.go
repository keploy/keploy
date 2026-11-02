//go:build !linux

package stall

import "time"

// threadWaits: no per-thread run delay or steal to read outside Linux.
type threadWaits struct{}

func (*threadWaits) longest() (time.Duration, uint64) { return 0, 0 }

// threadRunDelay: no run delay to read outside Linux.
func threadRunDelay() (time.Duration, bool) { return 0, false }

// sleepFor sleeps for d on a timer, and says whether stop closed first.
func sleepFor(d time.Duration, stop <-chan struct{}) (stopped bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
		return true
	case <-t.C:
		return false
	}
}
