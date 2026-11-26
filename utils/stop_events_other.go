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

const defaultNonLinuxStopStallBudget = 5 * time.Second

// StopStallBudget, off Linux, is the grace a stop by events gives the app
// when no compose-specific grace applies.
var StopStallBudget = defaultNonLinuxStopStallBudget

// ComposeStopGrace, off Linux, is Docker's standard 10s stop grace scoped
// strictly to compose teardown when StopStallBudget is at its 5s default; if a
// caller or test changed StopStallBudget, that value stands.
func ComposeStopGrace() time.Duration {
	if StopStallBudget == defaultNonLinuxStopStallBudget {
		return 10 * time.Second
	}
	return StopStallBudget
}

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
func WaitExitByEvents(logger *zap.Logger, exited <-chan struct{}, procs []Reached, moved *TeardownScope) (end StopEnd, bound string) {
	return WaitExitOffered(logger, exited, procs, moved, nil, OfferKillNow)
}

// WaitExitOffered is WaitExitByEvents here: no offer to end at a Ctrl+C is
// made, as no wait here outlasts its grace.
func WaitExitOffered(logger *zap.Logger, exited <-chan struct{}, procs []Reached, moved *TeardownScope, notice *StopNowNotice, offer string) (end StopEnd, bound string) {
	return WaitExitOfferedWithBudget(logger, exited, procs, moved, notice, offer, StopStallBudget)
}

// WaitExitOfferedWithBudget is WaitExitOffered with an explicit grace budget
// (for example a container's declared StopTimeout / terminationGracePeriod).
func WaitExitOfferedWithBudget(_ *zap.Logger, exited <-chan struct{}, _ []Reached, _ *TeardownScope, _ *StopNowNotice, _ string, budget time.Duration) (end StopEnd, bound string) {
	if budget <= 0 {
		budget = StopStallBudget
	}
	grace := time.NewTimer(budget)
	defer grace.Stop()
	select {
	case <-exited:
		return StopExited, budget.String()
	case <-grace.C:
		return StopStalled, budget.String()
	}
}
