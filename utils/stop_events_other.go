//go:build !linux

package utils

import "go.uber.org/zap"

// Off Linux a stop waits TreeInterruptGrace (a docker run app: its grace):
// the processes' progress, which the stop by events reads, is not read here,
// and nothing below is reached (the app's stopsByEvents is false).

// StopStallBudget is the stall bound of a stop by events, unused here.
var StopStallBudget = TreeInterruptGrace

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

// WaitExitByEvents waits for exited alone here.
func WaitExitByEvents(_ *zap.Logger, exited <-chan struct{}, _ []Reached) StopEnd {
	<-exited
	return StopExited
}
