package pkg

import (
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// WindowServe is how the agent serves one mock while a test window is open:
// the pool it hands the mock to, or none (ServeInWindow).
type WindowServe uint8

const (
	// NotServed: the window leaves the mock out. It is another window's, or
	// one no window serves (its response stamped before its request, a
	// window-bound mock outside its own window, a stated per-test mock
	// outside its window in either mode).
	NotServed WindowServe = iota
	// ServedPerTest: the window's per-test pool, its own test's mocks. Placed
	// in the window (models.PlaceOf, ends included), in flight when it began
	// (models.InFlightEligible, models.SentInGapAnsweredBy), with no recorded
	// time (which every window's pool gets), or a window-bound mock recorded
	// in it.
	ServedPerTest
	// ServedReleased: the window is the first that reaches the mock by a
	// release rule of the recorded windows (ServeWindow.Recorded). A
	// carry-over mock, which the carry-over tier keeps reachable from there
	// until it is consumed (models.WindowSchedule.CarryOverReachable). Or a
	// server push the set's staging files (models.IsStagedAtStaging), which
	// the push hub delivers from there (models.WindowSchedule.Released).
	ServedReleased
	// ServedStartup: the startup tier, which every window reads. A per-test
	// or window-bound mock placed before the first window.
	ServedStartup
	// ServedSession: the session pool, which every window reads. A session
	// or connection mock, one the recorder tagged config or connection, a
	// session-pool mock of the set's file, or a per-test mock whose recorder
	// stated no lifetime, outside the window in lax mode.
	ServedSession
)

// ServeWindow is the test window the agent has open, with what its window
// filter reads beside it (FilterPerTestAndLaxPromotedInFlightAware).
type ServeWindow struct {
	// Start and End are the window, MockFilterParams.AfterTime and
	// BeforeTime.
	Start, End time.Time
	// FirstStart is the first window's start: a mock placed before it is
	// startup traffic. Zero: no startup band.
	FirstStart time.Time
	// InFlightFrom is the start of the gap before the window
	// (models.WindowSchedule.GapBefore), exclusive. Zero: no call is in
	// flight into it.
	InFlightFrom time.Time
	// StrictPerCall is the filter's strict flag, the agent's agentStrict:
	// false on a windowed proxy, whose mock manager does the strict
	// filtering.
	StrictPerCall bool
	// StrictWindow is the user's window mode, MockFilterParams.
	// StrictMockWindow (KEPLOY_STRICT_MOCK_WINDOW=0 turns it off).
	StrictWindow bool
	// Recorded is the set's recorded windows, by which the agent releases
	// carry-over mocks and server pushes. Nil: those rules are not asked,
	// as in the window filter's own call; the carry-over tier and the push
	// hub serve those mocks apart from it.
	Recorded *models.WindowSchedule
	// Lookahead says the agent's per-test load runs the carry-over
	// lookahead for the window (the agent's perTestLoadMode: the strict
	// window mode, or a load by the test's mapping): it hands the mock
	// manager the carry-over mocks of the range CarryOverLoadRange names
	// past the window filter, and the manager files them into its carry-over
	// tier, whether or not their recorder stated a lifetime. Without it the
	// filter alone routes a carry-over mock, and promotes an unstated one
	// placed outside the window to the session pool in lax mode. Asked only
	// with Recorded.
	Lookahead bool
	// split says m is of a per-test slice a window filter has split by
	// lifetime already, the mock manager's (StageInWindow): the rules that
	// route a mock to the session pool by what it is, not where it is placed
	// (a session or connection lifetime, a config or connection tag), are
	// not asked again.
	split bool
}

// ServeInWindow is how the agent serves m, a mock of the set's per-test pool
// (session false) or its session pool (session true), while w is open. It is
// the agent's own decision: the window filter routes each mock by it
// (filterByTimeStampInFlightAware, for both pools), the mock manager stages
// what the filter hands it by it (StageInWindow), and the replayer's mock
// prune asks it which mocks a test that got no answer is served.
//
// The session pool's mocks all go to the session pool, unless the filter
// leaves one out; the per-test pool's go where the filter routes them.
func ServeInWindow(m *models.Mock, session bool, w ServeWindow) WindowServe {
	if m == nil {
		return NotServed
	}
	if session {
		// The agent filters its session pool with no call in flight, in the
		// strict-window off mode, and hands both slices to the session pool
		// (FilterConfigMocksTierAware).
		w.InFlightFrom, w.StrictWindow, w.Recorded = time.Time{}, false, nil
		if serveRuleOf(nil, m, w).serve() == NotServed {
			return NotServed
		}
		return ServedSession
	}
	return serveRuleOf(nil, m, w).serve()
}

// StageInWindow is how the mock manager stages m, a mock of the per-test slice
// it is handed, as window w opens (MockManager.SetMocksWithWindow): the pool
// ServeInWindow names, by the same rule (serveRuleOf), so that the manager
// and the replayer's mock prune cannot drift apart. The manager filters
// strictly what the agent's window filter hands it (w.StrictPerCall set):
// that filter has applied the user's window mode, and on a windowed proxy it
// is never strict. The filter has split its slices by lifetime too, so the
// lifetime and tag rules are not asked again: the manager places what it is
// handed as per-test by its recorded time, whatever its lifetime (a
// session-lifetime mock placed before the first window is startup traffic).
// logger says when m's response is stamped before its request, as the
// filter's does; answeredBeforeAsked reports that rule, which the manager
// counts apart from the out-of-window drops.
func StageInWindow(logger *zap.Logger, m *models.Mock, w ServeWindow) (serve WindowServe, answeredBeforeAsked bool) {
	if m == nil {
		return NotServed, false
	}
	w.split = true
	r := serveRuleOf(logger, m, w)
	return r.serve(), r == ruleAnsweredBeforeAsked
}

// serveRule is the rule of the window filter that routes a mock.
type serveRule uint8

const (
	ruleWindowBoundDropped serveRule = iota
	ruleWindowBoundInWindow
	ruleWindowBoundStartup
	ruleUntimed
	ruleSessionLifetime
	ruleAnsweredBeforeAsked
	ruleTaggedConfig
	ruleTaggedConnection
	ruleInWindow
	ruleStartup
	ruleInFlight
	ruleReleased
	ruleOutOfWindow
	ruleLaxPromoted
)

// serve is the pool a rule hands its mock to.
func (r serveRule) serve() WindowServe {
	switch r {
	case ruleWindowBoundInWindow, ruleUntimed, ruleInWindow, ruleInFlight:
		return ServedPerTest
	case ruleWindowBoundStartup, ruleStartup:
		return ServedStartup
	case ruleSessionLifetime, ruleTaggedConfig, ruleTaggedConnection, ruleLaxPromoted:
		return ServedSession
	case ruleReleased:
		return ServedReleased
	}
	return NotServed
}

// serveRuleOf is the rule that routes p while w is open, in the order the
// window filter applies them; the comments at filterByTimeStampInFlightAware
// say why each is where it is. logger says when p's response is stamped
// before its request (LeaveOutAnsweredBeforeAsked); nil says nothing.
func serveRuleOf(logger *zap.Logger, p *models.Mock, w ServeWindow) serveRule {
	if models.WindowBound(p.Kind) {
		switch placeWindowBound(p, w.Start, w.End, w.FirstStart) {
		case windowBoundInWindow:
			return ruleWindowBoundInWindow
		case windowBoundStartup:
			return ruleWindowBoundStartup
		}
		return ruleWindowBoundDropped
	}
	if untimedMock(p) {
		return ruleUntimed
	}
	req, res := p.Spec.ReqTimestampMock, p.Spec.ResTimestampMock
	if !w.split && (p.TestModeInfo.Lifetime == models.LifetimeSession || p.TestModeInfo.Lifetime == models.LifetimeConnection) {
		return ruleSessionLifetime
	}
	if LeaveOutAnsweredBeforeAsked(logger, p) {
		return ruleAnsweredBeforeAsked
	}
	if !w.split && p.Spec.Metadata != nil && !p.StatesLifetime() {
		switch p.Spec.Metadata["type"] {
		case "config":
			return ruleTaggedConfig
		case "connection":
			return ruleTaggedConnection
		}
	}
	if placedIn(p, w) {
		return ruleInWindow
	}
	if !w.FirstStart.IsZero() && models.PlaceOf(p).Before(w.FirstStart) {
		return ruleStartup
	}
	if !w.InFlightFrom.IsZero() && (strictWindowEnabled(w.StrictWindow) || p.StatesLifetime()) && models.InFlightEligible(p) &&
		models.SentInGapAnsweredBy(req, res, w.InFlightFrom, w.Start) {
		return ruleInFlight
	}
	// The push hub delivers a server push the staging filed from its
	// release window on, whatever the window mode: it pushes it, no
	// request of the app's asks for it.
	if w.Recorded != nil && models.IsStagedAtStaging(p) && w.Recorded.ReleasedFirstAt(req, w.Start) {
		return ruleReleased
	}
	// The carry-over tier reaches a carry-over mock from the release window
	// of its recorded time less the lookahead, where the filter drops it.
	// The tier is handed an unstated one too when the agent's load runs the
	// lookahead (w.Lookahead): the agent's default, the strict window mode
	// over its on-disk store, parks every timed per-test mock there and loads
	// only the window's, the startup band, the calls in flight and the
	// lookahead, so no filter sees it out of its window to promote it. Where
	// the filter is lax and handed it (lax mode, the resident store), it
	// promotes it to the session pool instead, which the hold's caller asks
	// apart (Lookahead false).
	carry := strictWindowEnabled(w.StrictPerCall) || p.StatesLifetime()
	if w.Recorded != nil && (carry || w.Lookahead) && models.IsCarryOver(p) && w.Recorded.CarryOverReachedFirstAt(req, w.Start) {
		return ruleReleased
	}
	if carry {
		return ruleOutOfWindow
	}
	return ruleLaxPromoted
}

// untimedMock reports whether p has no recorded request or response time,
// which every window's per-test pool is served.
func untimedMock(p *models.Mock) bool {
	return p.Spec.ReqTimestampMock.IsZero() || p.Spec.ResTimestampMock.IsZero()
}

// placedIn reports whether p, recorded with both times, is placed in w
// (models.PlaceOf, both ends included).
func placedIn(p *models.Mock, w ServeWindow) bool {
	place := models.PlaceOf(p)
	return !place.Before(w.Start) && !place.After(w.End)
}

// RecordedInWindow reports whether m has no recorded time or is placed in w,
// ends included: the placements by which serveRuleOf serves a mock to the
// window's own per-test pool (ruleUntimed, ruleInWindow), asked of any mock
// whatever its pool or lifetime. The replayer's mock prune holds a mock of
// the session pool for a test that got no answer by it.
func RecordedInWindow(m *models.Mock, w ServeWindow) bool {
	if m == nil {
		return false
	}
	return untimedMock(m) || placedIn(m, w)
}
