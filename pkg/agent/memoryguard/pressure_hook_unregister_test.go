package memoryguard

import "testing"

// TestRegisterPressureHookUnregister verifies that RegisterPressureHook returns
// a working deregistration func: the hook stops firing after unregister and is
// actually removed from the registry (no leak), and a double-unregister is a
// no-op. This is the contract the multi-app composer relies on to detach a
// per-session manager's hook when the session ends.
func TestRegisterPressureHookUnregister(t *testing.T) {
	t.Cleanup(resetAllPressure)

	pressureHookMu.RLock()
	base := len(pressureHooks)
	pressureHookMu.RUnlock()

	var aCalls, bCalls int
	unregA := RegisterPressureHook(func(bool) { aCalls++ })
	unregB := RegisterPressureHook(func(bool) { bCalls++ })
	t.Cleanup(unregA)
	t.Cleanup(unregB)

	pressureHookMu.RLock()
	got := len(pressureHooks)
	pressureHookMu.RUnlock()
	if got != base+2 {
		t.Fatalf("expected %d hooks after registering 2, got %d", base+2, got)
	}

	applyPausedState(true)
	if aCalls != 1 || bCalls != 1 {
		t.Fatalf("both hooks should fire once: aCalls=%d bCalls=%d", aCalls, bCalls)
	}

	// Unregister A: the registry must shrink and A must stop firing.
	unregA()
	pressureHookMu.RLock()
	got = len(pressureHooks)
	pressureHookMu.RUnlock()
	if got != base+1 {
		t.Fatalf("unregister did not remove the hook: got %d, want %d", got, base+1)
	}

	applyPausedState(false)
	if aCalls != 1 {
		t.Fatalf("unregistered hook A still fired: aCalls=%d", aCalls)
	}
	if bCalls != 2 {
		t.Fatalf("hook B should have fired again: bCalls=%d", bCalls)
	}

	// Idempotent: a second unregister of A must not change the count.
	unregA()
	pressureHookMu.RLock()
	got = len(pressureHooks)
	pressureHookMu.RUnlock()
	if got != base+1 {
		t.Fatalf("double-unregister changed hook count: got %d, want %d", got, base+1)
	}
}

// TestRegisterPressureHookNil verifies a nil hook is rejected and its
// returned unregister is a safe no-op.
func TestRegisterPressureHookNil(t *testing.T) {
	pressureHookMu.RLock()
	base := len(pressureHooks)
	pressureHookMu.RUnlock()

	unreg := RegisterPressureHook(nil)
	unreg() // must not panic

	pressureHookMu.RLock()
	got := len(pressureHooks)
	pressureHookMu.RUnlock()
	if got != base {
		t.Fatalf("nil hook mutated the registry: got %d, want %d", got, base)
	}
}
