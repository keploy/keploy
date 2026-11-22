package manager

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A parser's held exchanges are pending for the recording's stop until it has
// handed them on; the stop's first ask tells every holder, and one that holds
// after it is told at once.
func TestHeldExchangesArePendingForTheStopUntilHandedOn(t *testing.T) {
	m := New(zap.NewNop())
	m.SetWatermark(&backlogWatermark{})
	t0 := time.Unix(5000, 0)
	stopping, handedOn := m.HoldExchanges(t0)
	if closed(stopping) {
		t.Fatal("the holder was told to stop before the stop began")
	}
	if st := m.BacklogBefore(t0.Add(-time.Second)); st.Pending {
		t.Fatalf("exchanges held from after the stop's time are pending for it: %+v", st)
	}
	if !closed(stopping) {
		t.Fatal("the stop's ask did not tell the holder")
	}
	st := m.BacklogBefore(t0.Add(time.Second))
	if !st.Known || !st.Pending || !strings.Contains(st.WaitingOn, "holds exchanges") {
		t.Fatalf("held exchanges from before the stop: %+v, want pending, saying so", st)
	}
	handedOn()
	handedOn()
	if st := m.BacklogBefore(t0.Add(time.Second)); st.Pending {
		t.Fatalf("handed on, still pending: %+v", st)
	}
	late, lateHandedOn := m.HoldExchanges(t0)
	if !closed(late) {
		t.Fatal("a hold after the stop began was not told at once")
	}
	lateHandedOn()
	var nilMgr *SyncMockManager
	s, d := nilMgr.HoldExchanges(t0)
	d()
	if s != nil {
		t.Fatal("a nil manager tells of a stop")
	}
}

// A re-record (a new output channel) starts a new hold signal: its holds wait
// for its own stop, while a hold from the recording before keeps the channel
// that one's stop closed.
func TestANewRecordingsHoldsWaitForItsOwnStop(t *testing.T) {
	m := New(zap.NewNop())
	m.SetWatermark(&backlogWatermark{})
	m.SetOutputChannel(make(chan *models.Mock, 1))
	t0 := time.Unix(5000, 0)
	old, oldHandedOn := m.HoldExchanges(t0)
	m.BacklogBefore(t0.Add(time.Second)) // the first recording's stop
	if !closed(old) {
		t.Fatal("the first recording's stop did not tell its hold")
	}
	m.SetOutputChannel(make(chan *models.Mock, 1)) // the next recording
	next, nextHandedOn := m.HoldExchanges(t0.Add(time.Minute))
	if closed(next) {
		t.Fatal("a hold of the next recording is told its stop has begun")
	}
	if !closed(old) {
		t.Fatal("the hold from before lost the stop it was told")
	}
	oldHandedOn()
	m.BacklogBefore(t0.Add(2 * time.Minute)) // the next recording's stop
	if !closed(next) {
		t.Fatal("the next recording's stop did not tell its hold")
	}
	nextHandedOn()
}

// HoldsSession is the stop signal a hold made now takes: none before the
// first hold, the same until a re-record follows a stop, and a new one then.
func TestHoldsSessionIsTheSignalAHoldTakes(t *testing.T) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a nil manager's HoldsSession panicked: %v", r)
			}
		}()
		if (*SyncMockManager)(nil).HoldsSession() != nil {
			t.Fatal("a nil manager has a hold signal")
		}
	}()
	m := New(zap.NewNop())
	m.SetWatermark(&backlogWatermark{})
	m.SetOutputChannel(make(chan *models.Mock, 1))
	if m.HoldsSession() != nil {
		t.Fatal("a hold signal before any hold")
	}
	t0 := time.Unix(5000, 0)
	first, handedOn := m.HoldExchanges(t0)
	if m.HoldsSession() != first {
		t.Fatal("the signal is not the one the hold took")
	}
	m.BacklogBefore(t0.Add(time.Second)) // the stop
	handedOn()
	if m.HoldsSession() != first {
		t.Fatal("the signal changed at the stop, before any re-record")
	}
	m.SetOutputChannel(make(chan *models.Mock, 1)) // the re-record
	next, nextHandedOn := m.HoldExchanges(t0.Add(time.Minute))
	defer nextHandedOn()
	if s := m.HoldsSession(); s == first || s != next {
		t.Fatal("the re-record's signal is not the one its holds take")
	}
}
