package util

import (
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestRecoverWithoutCloseCatches asserts that a panic raised inside
// a function that defers RecoverWithoutClose is swallowed and the
// outer caller continues normally.
func TestRecoverWithoutCloseCatches(t *testing.T) {
	logger := zap.NewNop()

	ran := false
	func() {
		defer func() {
			// This outer deferred recover should NOT fire — the
			// inner RecoverWithoutClose must have already absorbed
			// the panic.
			if r := recover(); r != nil {
				t.Fatalf("panic leaked past RecoverWithoutClose: %v", r)
			}
		}()
		func() {
			defer RecoverWithoutClose(logger)
			panic("boom")
		}()
		ran = true
	}()

	if !ran {
		t.Fatalf("code after the panicking inner function did not run; panic was not recovered")
	}
}

// TestRecoverWithoutCloseNoPanic ensures RecoverWithoutClose is a
// zero-cost no-op on the happy path (no panic in flight).
func TestRecoverWithoutCloseNoPanic(t *testing.T) {
	logger := zap.NewNop()
	func() {
		defer RecoverWithoutClose(logger)
		// no panic
	}()
}

// TestRecoverWithoutCloseNilLoggerSafe asserts that passing a nil
// logger does not itself panic. Mirrors the safe-fallback contract
// of the existing Recover helper.
func TestRecoverWithoutCloseNilLoggerSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RecoverWithoutClose(nil) should not panic, got: %v", r)
		}
	}()

	// Case 1: nil logger, no panic in flight.
	func() {
		defer RecoverWithoutClose(nil)
	}()

	// Case 2: nil logger WITH a panic in flight. The helper must
	// still absorb the panic instead of letting it propagate.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic leaked past RecoverWithoutClose(nil): %v", r)
			}
		}()
		func() {
			defer RecoverWithoutClose(nil)
			panic("no-logger boom")
		}()
	}()
}

// TestRecoverWithoutCloseLogsRecovery uses a zap observer to assert
// that RecoverWithoutClose emits a log line on panic. We do NOT
// assert the exact HandleRecovery output (that lives in another
// package and its format is an implementation detail) — we only
// assert that SOMETHING at Error level is logged so regressions
// that silently drop panic reporting are caught.
func TestRecoverWithoutCloseLogsRecovery(t *testing.T) {
	core, recorded := observer.New(zap.ErrorLevel)
	logger := zap.New(core)

	func() {
		defer RecoverWithoutClose(logger)
		panic("observable boom")
	}()

	if recorded.Len() == 0 {
		t.Fatalf("RecoverWithoutClose did not emit any Error-level log lines on panic")
	}

	// At least one log line should reference the recovery. We
	// don't pin exact wording; matching on "recover" case-
	// insensitively is robust across rewording.
	var sawRecover bool
	for _, entry := range recorded.All() {
		if strings.Contains(strings.ToLower(entry.Message), "recover") {
			sawRecover = true
			break
		}
	}
	if !sawRecover {
		t.Fatalf("no log line mentioned recovery; saw: %v", recorded.All())
	}
}

// TestRecoverWithoutCloseConcurrent hammers the helper from many
// goroutines with panics to verify there's no shared-state race or
// goroutine leak introduced by the Sentry flush path.
func TestRecoverWithoutCloseConcurrent(t *testing.T) {
	logger := zap.NewNop()

	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			func() {
				defer RecoverWithoutClose(logger)
				panic("concurrent boom")
			}()
		}()
	}
	wg.Wait()
}
