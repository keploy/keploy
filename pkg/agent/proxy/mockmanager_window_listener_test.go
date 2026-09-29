package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

var _ integrations.TestWindowListener = (*MockManager)(nil)
var _ integrations.TestWindowListener = (*scopedMockDb)(nil)

type windowCall struct{ start, end time.Time }

// TestOnTestWindow pins when the listener runs: once per real window
// published by SetMocksWithWindow or a non-zero SetCurrentTestWindow, never
// for the BaseTime staging call or a clear, and not after unregister.
func TestOnTestWindow(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	var calls []windowCall
	unregister := mm.OnTestWindow(func(start, end time.Time) { calls = append(calls, windowCall{start, end}) })

	t0 := time.Date(2026, 9, 29, 5, 50, 29, 0, time.UTC)
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if len(calls) != 0 {
		t.Fatalf("staging call notified: %v", calls)
	}
	mm.SetMocksWithWindow(nil, nil, t0, t0.Add(10*time.Millisecond))
	mm.SetCurrentTestWindow(time.Time{}, time.Time{})
	mm.SetCurrentTestWindow(t0.Add(time.Second), t0.Add(2*time.Second))
	want := []windowCall{{t0, t0.Add(10 * time.Millisecond)}, {t0.Add(time.Second), t0.Add(2 * time.Second)}}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("calls = %v, want %v", calls, want)
	}

	unregister()
	mm.SetMocksWithWindow(nil, nil, t0.Add(3*time.Second), t0.Add(4*time.Second))
	if len(calls) != len(want) {
		t.Fatalf("notified after unregister: %v", calls)
	}
	if mm.OnTestWindow(nil) == nil {
		t.Fatal("nil fn must still return an unregister func")
	}
}

// TestOnTestWindow_RunsWithNoLockHeld: a listener that reads the pool back
// (GetPerTestMocksInWindow takes swapMu.RLock) must not deadlock, and sees the
// window it was told about.
func TestOnTestWindow_RunsWithNoLockHeld(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t0 := time.Date(2026, 9, 29, 5, 50, 29, 0, time.UTC)
	seen := make(chan time.Time, 1)
	mm.OnTestWindow(func(_, _ time.Time) {
		_, _ = mm.GetPerTestMocksInWindow()
		s, _ := mm.CurrentTestWindow()
		seen <- s
	})
	done := make(chan struct{})
	go func() {
		mm.SetMocksWithWindow(nil, nil, t0, t0.Add(time.Millisecond))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SetMocksWithWindow did not return: listener ran under a MockManager lock")
	}
	if s := <-seen; !s.Equal(t0) {
		t.Fatalf("listener saw window start %v, want %v", s, t0)
	}
}

// TestScopedMockDbForwardsOnTestWindow: a scoped worker's parser registers
// through the wrap and is still told about windows set on the manager.
func TestScopedMockDbForwardsOnTestWindow(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	s := &scopedMockDb{MockMemDb: mm, allow: map[string]struct{}{}}
	n := 0
	unregister := s.OnTestWindow(func(_, _ time.Time) { n++ })
	t0 := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	mm.SetMocksWithWindow(nil, nil, t0, t0.Add(time.Millisecond))
	unregister()
	mm.SetMocksWithWindow(nil, nil, t0.Add(time.Second), t0.Add(2*time.Second))
	if n != 1 {
		t.Fatalf("scoped listener notified %d times, want 1", n)
	}
}
