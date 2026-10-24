//go:build !linux

package utils

import (
	"time"

	"go.uber.org/zap"
)

// Off Linux the processes' progress, which the stop by events reads, is not
// read. A native app's and a docker run app's stops keep their graces there
// (the app's stopsByEvents is false). A docker compose app's stop (P101-c) is
// a wait for its container's stop events here too, bounded by its grace.

// StopStallBudget, off Linux, is the grace a stop by events gives the app:
// the grace keploy's stop gave a docker app (graceBudget, pkg/client/app).
var StopStallBudget = 5 * time.Second

// StopStallSaid is how a kill names the bound it killed on: wall time here.
func StopStallSaid() string { return StopStallBudget.String() }

// TreeOf reads no process here.
func TreeOf(int) []Reached { return nil }

// ProcOf reads no process here.
func ProcOf(int) (Reached, bool) { return Reached{}, false }

// ExitOf cannot watch a process here: it says it exited.
func ExitOf(Reached) (<-chan struct{}, func()) {
	c := make(chan struct{})
	close(c)
	return c, func() {}
}

// WaitExitByEvents waits for exited for StopStallBudget here, where no
// progress can be read, and then says a kill is due; bound is StopStallSaid.
func WaitExitByEvents(_ *zap.Logger, exited <-chan struct{}, _ []Reached, _ *TeardownScope) (end StopEnd, bound string) {
	grace := time.NewTimer(StopStallBudget)
	defer grace.Stop()
	select {
	case <-exited:
		return StopExited, StopStallSaid()
	case <-grace.C:
		return StopStalled, StopStallSaid()
	}
}

// WaitStopByEvents is WaitExitByEvents here: no process is read, so none is
// said to be gone.
func WaitStopByEvents(logger *zap.Logger, stopped <-chan struct{}, procs []Reached, moved *TeardownScope) (end StopEnd, bound string, gone bool) {
	end, bound = WaitExitByEvents(logger, stopped, procs, moved)
	return end, bound, false
}
