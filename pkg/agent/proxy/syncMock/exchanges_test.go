package manager

import (
	"testing"
	"time"
)

// An exchange open from before a stop keeps it draining until it ends, and for
// no longer than OpenExchangeWait past the stop.
func TestExchanges_OpenBefore(t *testing.T) {
	var e Exchanges
	stop := time.Unix(1_700_000_000, 0)
	if e.OpenBefore(stop, stop) {
		t.Fatal("pending with nothing open")
	}
	done := e.Open(stop.Add(-time.Second))
	if !e.OpenBefore(stop, stop) {
		t.Fatal("an exchange open from before the stop is not pending")
	}
	if e.OpenBefore(stop.Add(-2*time.Second), stop) {
		t.Fatal("an exchange that started after the time asked about is pending")
	}
	if e.OpenBefore(stop, stop.Add(OpenExchangeWait)) {
		t.Fatal("an exchange open past OpenExchangeWait is still waited for")
	}
	done()
	done() // a second call is a no-op
	if e.OpenBefore(stop, stop) {
		t.Fatal("an ended exchange is still pending")
	}
}

// A zero start is taken as now, not as the start of time, which would make it
// pending to any stop.
func TestExchanges_ZeroStartIsNow(t *testing.T) {
	var e Exchanges
	before := time.Now().Add(-time.Second)
	defer e.Open(time.Time{})()
	if e.OpenBefore(before, before) {
		t.Fatal("an exchange with no start is pending to a stop before it opened")
	}
}

// A capture with no watermark cannot tell what it has yet to hand over, except
// an exchange it has open from before the stop, which is pending.
func TestPendingBefore_NoWatermarkTellsOpenExchanges(t *testing.T) {
	m := &SyncMockManager{}
	stop := time.Now()
	if pending, known := m.PendingBefore(stop); known || pending {
		t.Fatalf("nothing open: pending=%v known=%v, want false false (cannot tell)", pending, known)
	}
	done := m.OpenExchange(stop.Add(-time.Second))
	defer done()
	if pending, known := m.PendingBefore(stop); !known || !pending {
		t.Fatalf("an exchange open from before: pending=%v known=%v, want true true", pending, known)
	}
}
