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
	// ended is, by the index of a window several tests were merged into
	// (models.TestWindow.Tests), those of them that have ended: its end is
	// reached once each has.
	ended map[int]map[string]bool
	// atChange are the idle marks' releases of the parsers that wait for the
	// next change (IdleUntilRunPlaceChanges): the change runs them.
	atChange []func()
}

// RunPlace is the run's place now (integrations.RunPlacer): the anchor it
// has reached among the windows the replay seeded for the staged set.
//
// A closed manager is at the set's end: nothing it served waits any longer.
func (m *MockManager) RunPlace() models.RunPlace {
	m.place.mu.Lock()
	p := models.RunPlace{Staging: m.place.gen, Reached: m.place.reached, SetEnd: m.place.setEnd}
	m.place.mu.Unlock()
	p.Windows = m.placeSchedule()
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

// signalRunPlaceLocked wakes every RunPlaceChanged waiter, and hands back
// the idle marks' releases of those that wait (IdleUntilRunPlaceChanges),
// for the caller to run once it has released place.mu, before it returns.
// place.mu held.
func (m *MockManager) signalRunPlaceLocked() (release []func()) {
	if m.place.sig != nil {
		close(m.place.sig)
		m.place.sig = nil
	}
	release, m.place.atChange = m.place.atChange, nil
	return release
}

// runAll runs each of fs.
func runAll(fs []func()) {
	for _, f := range fs {
		f()
	}
}

// signalRunPlace wakes every RunPlaceChanged waiter: at Close, which they
// read as the set's end, and when the windows the place is among change.
func (m *MockManager) signalRunPlace() {
	m.place.mu.Lock()
	release := m.signalRunPlaceLocked()
	m.place.mu.Unlock()
	runAll(release)
}

// IdleUntilRunPlaceChanges marks a parser's wait for the next change of the
// place (integrations.RunPlacer): mark runs, under place.mu, only while
// changed is the channel RunPlaceChanged hands out now, and its release
// runs at the next change, before the call that makes it returns. Once the
// place has changed, ok is false and nothing is marked.
func (m *MockManager) IdleUntilRunPlaceChanges(changed <-chan struct{}, mark func() (release func())) (func(), bool) {
	m.place.mu.Lock()
	defer m.place.mu.Unlock()
	if changed == nil || changed != m.place.sig {
		return func() {}, false
	}
	var once sync.Once
	unmark := mark()
	release := func() { once.Do(unmark) }
	m.place.atChange = append(m.place.atChange, release)
	return release, true
}

// beginRunPlace begins the place of a set the replay stages (a
// models.BaseTime SetMocksWithWindow): its start-up, under a new Staging. A
// hold taken under the set before is over.
func (m *MockManager) beginRunPlace() {
	m.place.mu.Lock()
	m.place.gen++
	m.place.reached, m.place.setEnd, m.place.ended = 0, false, nil
	release := m.signalRunPlaceLocked()
	m.place.mu.Unlock()
	runAll(release)
}

// reachRunPlace moves the run's place to anchor, unless it is there or past
// it already, and to the set's end with setEnd.
func (m *MockManager) reachRunPlace(anchor int, setEnd bool) {
	var release []func()
	defer func() { runAll(release) }()
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
		release = m.signalRunPlaceLocked()
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
	if i := m.placeSchedule().CurrentIndex(start); i >= 0 {
		m.reachRunPlace(models.RunOpen(i), false)
	}
}

// reachTest notes that test's window opened (open) or that test ended
// (!open), by its name in the seeded schedule. A name it does not hold moves
// nothing. A window several tests were merged into (tests run at once) opens
// at the first of them to begin, and its end is reached once each of them
// has ended: before that, an end moves the place only to its open.
func (m *MockManager) reachTest(test string, open bool) {
	s := m.placeSchedule()
	i, ok := s.IndexOfTest(test)
	if !ok {
		return
	}
	if open {
		m.reachRunPlace(models.RunOpen(i), false)
		return
	}
	if members := s.Window(i).Tests; len(members) > 1 && !m.allEnded(i, test, members) {
		m.reachRunPlace(models.RunOpen(i), false)
		return
	}
	m.reachRunPlace(models.RunEnd(i), false)
}

// allEnded notes that test, one of the tests merged into window i (members),
// has ended, and reports whether each of them has.
func (m *MockManager) allEnded(i int, test string, members []string) bool {
	m.place.mu.Lock()
	defer m.place.mu.Unlock()
	if m.place.ended == nil {
		m.place.ended = map[int]map[string]bool{}
	}
	if m.place.ended[i] == nil {
		m.place.ended[i] = map[string]bool{}
	}
	m.place.ended[i][test] = true
	for _, t := range members {
		if !m.place.ended[i][t] {
			return false
		}
	}
	return true
}

// placeSchedule is the schedule the run's place is among: those seeded for
// the place (SeedRunPlaceWindows), else the set's recorded test windows
// (SeedRecordedWindows); nil where the replay seeded neither.
func (m *MockManager) placeSchedule() *models.WindowSchedule {
	if s := m.placeWindows.Load(); s != nil {
		return s
	}
	return m.testWindows.Load()
}

// SeedRunPlaceWindows installs the window of every test of the set a replay
// serves for the run's place alone: keploy mock replay's, read as
// models.RunWindows says, as its recorded windows (SeedRecordedWindows, under
// the proven switch) do not end at its tests' ends. Nothing else reads them,
// so a parser, the learn step and the window release rules behave as before.
// Parked at a set boundary until the set's staging installs it, as
// SeedRecordedWindows' are; none leaves the set none.
func (m *MockManager) SeedRunPlaceWindows(ws []models.TestWindow) {
	var s *models.WindowSchedule
	if len(ws) > 0 {
		s = models.NewWindowSchedule(ws)
	}
	m.swapMu.Lock()
	if m.boundaryPending {
		m.placeWindowsSeeded = s
	} else {
		m.placeWindows.Store(s)
	}
	m.swapMu.Unlock()
	m.signalRunPlace()
}

// ReachRunTest notes that the replay began test (open) or ended it (!open):
// keploy mock replay's scope begin and end, which with the proven switch off
// serve no exact view to move the place (serveExactTest, ServeExactPool).
func (m *MockManager) ReachRunTest(test string, open bool) { m.reachTest(test, open) }

// EndRunTest notes that the replay has ended test: it has the test's answer,
// or gave up on it. Answers recorded in the gap after it are due.
func (m *MockManager) EndRunTest(test string) { m.reachTest(test, false) }

// EndRunSet notes that the replay has reached the staged set's end: every
// held answer is due. keploy test's read of the set's end (Proxy.EndRunSet)
// makes it before that read settles the application's calls: a connection
// whose answer waited is busy from then until its parser has written it, so
// the read settles the calls the written answer makes, and takes their
// misses.
func (m *MockManager) EndRunSet() { m.reachRunPlace(0, true) }
