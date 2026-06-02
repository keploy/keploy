//go:build !windows

package util

import (
	"os"
	"os/signal"
	"syscall"
)

// installSignalHandler registers a SIGUSR1 handler that trips ks.
// Registered at most once per KillSwitch instance via sync.Once so
// re-using a KillSwitch across tests doesn't pile up goroutines or
// channels. The listener goroutine is intentionally leaked for the
// lifetime of the process — the normal pattern for signal handlers.
//
// Exposed-for-testing hook: [tripOnSignalForTest] lets the unix
// test file simulate a SIGUSR1 without actually sending one, which
// keeps tests deterministic on CI runners where self-signalling
// behaviour varies.
func installSignalHandler(ks *KillSwitch) {
	ks.signalOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				logSignalHandlerFailure(errFromRecover(r))
			}
		}()
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGUSR1)
		go func() {
			for range c {
				ks.Trip()
			}
		}()
	})
}

// tripOnSignalForTest invokes the signal-handler action directly.
// Tests use this instead of actually delivering SIGUSR1 because
// self-signalling can race with the test runner's own signal
// handling on some platforms.
func tripOnSignalForTest(ks *KillSwitch) {
	ks.Trip()
}

// errFromRecover converts a recovered panic value into an error
// for [logSignalHandlerFailure]. Signal wiring has historically
// never panicked in the Go runtime, but if that ever changed we'd
// rather emit a stderr line than crash proxy startup.
func errFromRecover(r interface{}) error {
	if err, ok := r.(error); ok {
		return err
	}
	return &panicError{v: r}
}

type panicError struct{ v interface{} }

func (p *panicError) Error() string { return itoa(p.v) }

func itoa(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return "panic of unknown type"
}
