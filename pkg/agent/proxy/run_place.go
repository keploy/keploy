package proxy

import (
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

var _ integrations.RunPlacer = (*MockManager)(nil)

// runPlace is where the replay's run is among the staged set's recorded
// windows (integrations.RunPlacer). It is fed by the events the replayer
// drives: a test's window opening (a per-test SetMocksWithWindow, a test's
// exact view), a test's end (the pool's exact view after it, EndRunTest) and
// the set's end (EndRunSet). The place never goes back within a set; a
// set's staging (a models.BaseTime SetMocksWithWindow) begins a new one. A
// keploy mock replay's per-test re-staging (SetScopeMocks) does not: it is
// the same set.
type runPlace struct {
	mu      sync.Mutex
	gen     uint64
	reached int
	setEnd  bool
	sig     chan struct{}
}

// RunPlace is the run's place now (integrations.RunPlacer): the anchor it
// has reached among the windows the replay seeded for the staged set.
//
// A closed manager is at the set's end: nothing it served waits any longer.
func (m *MockManager) RunPlace() models.RunPlace {
	m.place.mu.Lock()
	p := models.RunPlace{Staging: m.place.gen, Reached: m.place.reached, SetEnd: m.place.setEnd}
	m.place.mu.Unlock()
	p.Windows = m.testWindows.Load()
	if m.closed.Load() {
		p.SetEnd = true
	}
	return p
}

// RunPlaceChanged returns a channel closed at the next change of the run's
// place, at the next set's staging, and at Close (integrations.RunPlacer).
// Take it before reading RunPlace.
func (m *MockManager) RunPlaceChanged() <-chan struct{} {
	m.place.mu.Lock()
	defer m.place.mu.Unlock()
	if m.place.sig == nil {
		m.place.sig = make(chan struct{})
	}
	return m.place.sig
}

// signalRunPlaceLocked wakes every RunPlaceChanged waiter. place.mu held.
func (m *MockManager) signalRunPlaceLocked() {
	if m.place.sig != nil {
		close(m.place.sig)
		m.place.sig = nil
	}
}

// signalRunPlace wakes every RunPlaceChanged waiter: at Close, which they
// read as the set's end.
func (m *MockManager) signalRunPlace() {
	m.place.mu.Lock()
	m.signalRunPlaceLocked()
	m.place.mu.Unlock()
}

// beginRunPlace begins the place of a set the replay stages (a
// models.BaseTime SetMocksWithWindow): its start-up, under a new Staging. A
// hold taken under the set before is over.
func (m *MockManager) beginRunPlace() {
	m.place.mu.Lock()
	defer m.place.mu.Unlock()
	m.place.gen++
	m.place.reached, m.place.setEnd = 0, false
	m.signalRunPlaceLocked()
}

// reachRunPlace moves the run's place to anchor, unless it is there or past
// it already, and to the set's end with setEnd.
func (m *MockManager) reachRunPlace(anchor int, setEnd bool) {
	m.place.mu.Lock()
	defer m.place.mu.Unlock()
	moved := false
	if anchor > m.place.reached {
		m.place.reached = anchor
		moved = true
	}
	if setEnd && !m.place.setEnd {
		m.place.setEnd = true
		moved = true
	}
	if moved {
		m.signalRunPlaceLocked()
	}
}

// placeWindowAt moves the place for a SetMocksWithWindow whose window starts
// at start: a set's staging (models.BaseTime) begins a new place, and a
// test's window opens the window the seeded schedule starts last at or
// before start. A zero start moves nothing.
func (m *MockManager) placeWindowAt(start time.Time) {
	if start.IsZero() {
		return
	}
	if start.Equal(models.BaseTime) {
		m.beginRunPlace()
		return
	}
	if i := m.testWindows.Load().CurrentIndex(start); i >= 0 {
		m.reachRunPlace(models.RunOpen(i), false)
	}
}

// reachTest notes that test's window opened (open) or that test ended
// (!open), by its name in the seeded schedule. A name it does not hold moves
// nothing.
func (m *MockManager) reachTest(test string, open bool) {
	i, ok := m.testWindows.Load().IndexOfTest(test)
	if !ok {
		return
	}
	if open {
		m.reachRunPlace(models.RunOpen(i), false)
		return
	}
	m.reachRunPlace(models.RunEnd(i), false)
}

// EndRunTest notes that the replay has ended test: it has the test's answer,
// or gave up on it. Answers recorded in the gap after it are due.
func (m *MockManager) EndRunTest(test string) { m.reachTest(test, false) }

// EndRunSet notes that the replay has reached the staged set's end: every
// held answer is due.
func (m *MockManager) EndRunSet() { m.reachRunPlace(0, true) }
