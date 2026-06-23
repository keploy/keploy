// Package utils provides utility functions for the Keploy application.
package utils

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

var cancel context.CancelFunc

// preCancelHooks are callbacks fired by NewCtx's signal handler in the
// instant BEFORE cancel() is called. The handler runs them synchronously,
// in registration order, so subsystems can read live state (e.g. the
// agent's syncMock buffer length) at the exact moment of shutdown —
// before any goroutine has reacted to ctx cancellation.
//
// Use case (added for the mongo teardown-orphan investigation): the
// agent registers a hook that writes the syncMock buffer state to
// stderr via fmt.Fprintf (NOT zap, which is buffered and silently
// drops messages when the process dies before the next flush). This
// gives a definitive answer to "did the agent have unsent mocks at
// the moment of shutdown" without depending on the structured logger.
//
// Hooks MUST be fast (no blocking I/O, no network calls) — they run
// on the signal-delivery goroutine and any blocking work delays the
// cancellation and increases the chance of SIGKILL truncation.
var (
	preCancelMu    sync.Mutex
	preCancelHooks []func()
)

// RegisterPreCancelHook appends fn to the list of callbacks NewCtx's
// signal handler runs synchronously before cancel(). Safe to call
// from any goroutine at any time after NewCtx; hooks registered after
// the signal has fired will not run.
func RegisterPreCancelHook(fn func()) {
	if fn == nil {
		return
	}
	preCancelMu.Lock()
	preCancelHooks = append(preCancelHooks, fn)
	preCancelMu.Unlock()
}

func NewCtx() context.Context {
	// Create a context that can be canceled
	ctx, cancel := context.WithCancel(context.Background())

	SetCancel(cancel)
	// Set up a channel to listen for signals
	sigs := make(chan os.Signal, 1)
	// os.Interrupt is more portable than syscall.SIGINT
	// there is no equivalent for syscall.SIGTERM in os.Signal
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	// Start a goroutine that will cancel the context when a signal is received
	go func() {
		sig := <-sigs // this received signal will be inside keploy docker container if running in docker else on the host.
		fmt.Printf("Signal received: %s, canceling context...\n", sig)

		// App-managed graceful-shutdown drain (Kubernetes sidecar path).
		// When the injecting webhook runs in app-managed-drain mode it sets
		// KEPLOY_SIDECAR_DRAIN_SECONDS on the agent instead of a native
		// `sleep` lifecycle (SleepAction) preStop hook — that field is GA
		// only in k8s 1.30 and some older apiservers reject it, which would
		// fail pod admission. We honour the drain HERE, before cancel(): the
		// proxy and every parser goroutine are still live (ctx not yet
		// cancelled), so in-flight streams keep completing for the window —
		// exactly what a preStop sleep bought — and only then do we tear
		// down. A second signal cuts the wait short for an impatient
		// operator (or a kubelet escalating toward SIGKILL). Unset / zero
		// (the default, and every non-sidecar invocation) skips the wait
		// entirely, preserving the historical immediate-cancel behaviour.
		if d := sidecarDrainDuration(); d > 0 {
			fmt.Printf("Draining in-flight connections for %s before shutdown (KEPLOY_SIDECAR_DRAIN_SECONDS)...\n", d)
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case sig2 := <-sigs:
				t.Stop()
				fmt.Printf("Second signal received: %s, ending drain early...\n", sig2)
			}
		}

		// Run pre-cancel hooks SYNCHRONOUSLY while live state is still
		// readable. fmt.Fprintf to stderr inside hooks is the right
		// choice — it goes straight to the syscall and survives even
		// if the process is SIGKILL'd microseconds later (zap's async
		// logger does not survive that race). See RegisterPreCancelHook
		// doc comment for the investigation context.
		preCancelMu.Lock()
		hooks := append([]func(){}, preCancelHooks...)
		preCancelMu.Unlock()
		for _, fn := range hooks {
			func() {
				defer func() {
					_ = recover()
				}()
				fn()
			}()
		}

		cancel()
	}()

	return ctx
}

// Stop requires a reason to stop the server.
// this is to ensure that the server is not stopped accidentally.
// and to trace back the stopper
func Stop(logger *zap.Logger, reason string) error {
	// Stop the server.
	if logger == nil {
		return errors.New("logger is not set")
	}
	if cancel == nil {
		err := errors.New("cancel function is not set")
		LogError(logger, err, "failed stopping keploy")
		return err
	}

	if reason == "" {
		err := errors.New("cannot stop keploy without a reason")
		LogError(logger, err, "failed stopping keploy")
		return err
	}

	logger.Info("stopping Keploy", zap.String("reason", reason))
	ExecCancel()
	return nil
}

func ExecCancel() {
	cancel()
}

func SetCancel(c context.CancelFunc) {
	cancel = c
}

// sidecarDrainDuration returns the graceful-shutdown drain window the
// Kubernetes injecting webhook sets, in app-managed-drain mode, via the
// KEPLOY_SIDECAR_DRAIN_SECONDS env var. It is the in-process replacement for
// a native `sleep` lifecycle preStop hook on the keploy-agent sidecar.
//
// Returns 0 (no wait) when the var is unset, non-numeric, or non-positive —
// so only an explicit positive value can delay shutdown, and every
// non-sidecar / older-deployment invocation keeps the historical
// cancel-immediately-on-signal behaviour.
func sidecarDrainDuration() time.Duration {
	v := os.Getenv("KEPLOY_SIDECAR_DRAIN_SECONDS")
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
