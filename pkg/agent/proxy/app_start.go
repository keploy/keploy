package proxy

import (
	"errors"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A test set is served to one start of the application: its staging stages
// the set's start (the recordings made before its first test, and the rest
// of its pool), and what the application's start-up consumes of it is spent
// for the set. That holds wherever keploy starts the application for each
// set. It does not hold where something else starts it again within a set: a
// replay pod whose application container dies as it boots and is restarted
// by the kubelet. Its next instance makes the same start-up calls, and found
// them spent: every one a miss, and the instance died of it too.
//
// RearmAppStart serves the set's start again to such an instance, and
// BeforeServing is how a build that can tell a new instance of the
// application from the processes it sees asks for it, on the connection that
// shows it, before anything on that connection is served.

// appStartStaging is a set's start as its staging staged it (appStart).
type appStartStaging struct {
	filtered, unfiltered, startup []*models.Mock
	end                           time.Time
}

// ErrNoSetStart says RearmAppStart found no set's start to serve again: no
// test set is staged (or only by a staging that stages no set's start), or
// the next set's session began.
var ErrNoSetStart = errors.New("no test set's start is staged")

// ErrSetStarted says RearmAppStart found a test of the set run: the set's
// start is served again only to an instance of the application that starts
// before the set's first test, whose recordings nothing but the start-up has
// consumed. What a test consumed is that test's, and is never served again.
var ErrSetStarted = errors.New("a test of the set has run: keploy serves the set's start again only to an instance of the application that starts before the set's first test")

// noteAppStartLocked keeps what a staging stages as the set's start when
// atStart (a models.BaseTime staging, not a scope's, while no test of the set
// has run), and forgets it otherwise: once a test has run the set's start is
// not served again (RearmAppStart), and the trees a test's window replaces it
// with are what is in force. The slices are kept, not the mocks copied: a
// staged mock is not written after it is pooled (ownForStaging), and they are
// the mocks the trees hold. A staging the replay makes again before any test
// (its re-registration with an agent it took for replaced) replaces it with
// what it stages then. Caller holds swapMu.
func (m *MockManager) noteAppStartLocked(filtered, unfiltered, startup []*models.Mock, end time.Time, atStart bool) {
	if !atStart {
		m.appStart = nil
		return
	}
	m.appStart = &appStartStaging{
		filtered:   append([]*models.Mock(nil), filtered...),
		unfiltered: append([]*models.Mock(nil), unfiltered...),
		startup:    append([]*models.Mock(nil), startup...),
		end:        end,
	}
}

// appStartRearmableLocked says why the set's start cannot be served again
// now, nil when it can (RearmAppStart). Caller holds swapMu.
func (m *MockManager) appStartRearmableLocked() error {
	if m.boundaryPending {
		return ErrNoSetStart
	}
	if m.testFiredThisSet || m.scopeFiredThisSet || m.exact.fired.Load() {
		return ErrSetStarted
	}
	if m.appStart == nil {
		return ErrNoSetStart
	}
	return nil
}

// RearmAppStart serves the set's start again to a new instance of the
// application, which starts before any test of the set has run: as the
// set's staging served it, every one of its recordings unspent. It stages
// again, from models.BaseTime, what the set's staging staged (appStart),
// through that staging's own code: the trees are built afresh, a new staging
// epoch starts (so whatever the earlier instance consumed, outside every test
// under exact serving or out of its tier without it, is spent no more: the
// take log, the outside answers and the once-claims are the staging's), and
// every stateful cursor starts over, as the agent starts them over at the
// set's staging (ResetStatefulCursors). What it consumed as it started of the
// carry-over mocks is forgotten (bootTaken), and so are its serves
// (servedCount, the round's takes), which HasUnservedRecording reads. What
// the set's staging installed beside the trees stays as it is: the startup
// cutoff and the recorded windows, the exact view the agent installed and the
// pool it names, the background rules, the worker scope, the recording
// sources. Under the proven switch, what the earlier instance's start-up
// bound and served is not the new one's: as the set's own Mock does before
// its staging, the learn step begins a new set (the Proxy's re-arm hands that
// step, rearmAppStart), and a new variable set begins. What the earlier
// instance consumed stays in the consumed history the replay drains: the new
// instance consumes the same.
//
// It refuses once a test of the set has run (ErrSetStarted): the test's
// recordings are that test's, served once, and a set's start served again
// then would serve the start-up recordings in the window of the next test.
// ErrNoSetStart when no set's start is staged, or the next set's session
// began meanwhile. No test of the set opens while it runs: a test's window is
// staged, and an exact test opened, holding rearmMu, which it holds from its
// check to its staging.
func (m *MockManager) RearmAppStart() error { return m.rearmAppStart(nil) }

// rearmAppStart is RearmAppStart, with learnNewSet, when not nil, the learn
// step's new set it begins under the proven switch before the variables'
// (the Proxy's, for the run's manager alone: the learn step is the run's).
// learnNewSet refuses (ErrSetStarted) once a test of the set began on the
// learn step: the replay begins a test there before it stages the test's
// mocks, so no flag of the manager's says it yet. Then nothing is re-armed.
func (m *MockManager) rearmAppStart(learnNewSet func() error) error {
	m.rearmMu.Lock()
	defer m.rearmMu.Unlock()
	m.swapMu.RLock()
	err := m.appStartRearmableLocked()
	m.swapMu.RUnlock()
	if err != nil {
		return err
	}
	if provenswitch.On() {
		if learnNewSet != nil {
			if err := learnNewSet(); err != nil {
				return err
			}
		}
		m.NewVariableSet()
	}
	m.keepRunOnlyForAnExactStartUp(models.BaseTime)
	if m.testHookRearm != nil {
		m.testHookRearm()
	}
	m.swapMu.Lock()
	// A test cannot have run since the check (rearmMu), but the next set's
	// session may have begun (ResetForReplaySession, which takes swapMu):
	// the set's start is that session's to stage then.
	if err := m.appStartRearmableLocked(); err != nil {
		m.swapMu.Unlock()
		return err
	}
	start := m.appStart
	m.forgetAllBootTaken()
	m.consumedMu.Lock()
	if m.servedCount != nil {
		m.servedCount = make(map[string]uint64)
	}
	m.takenThisSession = nil
	m.consumedMu.Unlock()
	m.ResetStatefulCursors()
	filtered := append([]*models.Mock(nil), start.filtered...)
	unfiltered := append([]*models.Mock(nil), start.unfiltered...)
	startup := append([]*models.Mock(nil), start.startup...)
	ownForStaging(&filtered, &unfiltered, &startup)
	m.setMocksWithWindowLocked(filtered, unfiltered, startup, models.BaseTime, start.end, false)
	m.swapMu.Unlock()
	m.signalWindowChange()
	m.placeWindowAt(models.BaseTime)
	if m.logger != nil {
		m.logger.Info("a new instance of the application started before the test set's first test: its start is served again, as the set's staging served it",
			zap.Int("stagedMocks", len(filtered)+len(startup)), zap.Int("sessionMocks", len(unfiltered)))
	}
	return nil
}

// forgetAllBootTaken forgets every carry-over mock the application consumed
// as it started (bootTaken): a new instance of it starts again
// (RearmAppStart).
func (m *MockManager) forgetAllBootTaken() {
	m.bootTakenMu.Lock()
	m.bootTaken = nil
	m.bootTakenMu.Unlock()
}

// BeforeServing, when a build sets it, is handed the process of each
// connection the proxy accepts in a replay (keploy test), with rearm, in
// either state of the proven switch, on the connection's own goroutine,
// before anything on the connection is served. A build that tells from it
// that the process is a new instance of the application, started again
// within the test set (its earlier instance gone), calls rearm before it
// returns: the set's start is served again (MockManager.RearmAppStart) on
// the manager that serves the connection, and every call on the connection
// is served from it. It returns what RearmAppStart returned
// (ErrSetStarted, ErrNoSetStart), or why the connection has no manager.
//
// The connections of the new instance must wait for the decision: a build
// that lets one of them be served before rearm returned has it served from
// the set's start as the earlier instance left it. A build decides under a
// lock of its own, so a second connection of the instance waits for the
// first's rearm.
//
// Set it before the proxy starts: it is read without synchronisation.
var BeforeServing func(proc Process, rearm func() error)

// beforeServing hands BeforeServing the process proc of a connection served
// from the manager of the test worker instance inst (connManager).
func (p *Proxy) beforeServing(proc Process, inst uint32) {
	f := BeforeServing
	if f == nil {
		return
	}
	f(proc, func() error {
		m, err := p.connManager(inst)
		if err != nil {
			return err
		}
		if m == nil {
			return errNoMockManager
		}
		if m != p.getMockManager() {
			// A test worker's manager: the learn step is the run's, and
			// follows no worker's tests.
			return m.rearmAppStart(nil)
		}
		return m.rearmAppStart(p.learnNewSetForRearm)
	})
}

// learnNewSetForRearm begins the learn step's new set for a re-arm of the
// run's manager (rearmAppStart), as the set's Mock begins it, in one step
// with its check: refused (ErrSetStarted) once a test of the set began on the
// learn step (the replay's capture window opens a test there before its mocks
// are staged), so a re-arm never ends a test that has begun.
func (p *Proxy) learnNewSetForRearm() error {
	var err error
	p.learnBoundary(func(t *learn.Tracker) {
		if t == nil {
			return
		}
		if _, _, began := t.State(); began {
			err = ErrSetStarted
			return
		}
		t.NewSet()
	})
	return err
}
