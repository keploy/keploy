package manager

import (
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

// MaxHeldBytes is the hold's budget: the most the mock managers of one process
// hold for their requests in flight (SyncMockManager.held, OpenWindow),
// together, by SIZE (mockSize: about the heap a mock takes). It is one budget
// for the process, not one per manager: a DaemonSet agent runs a manager per
// recorded app (New), besides the package's own (Get), and no memory guard,
// so a budget per manager would let the hold grow with the number of apps.
//
// Only mocks a reaper would have dropped are held: past the stale horizon, or
// a static-dedup duplicate's leftovers, while a window still open may own
// them. Mocks are attributed by time alone, so while a request is in flight
// every per-test mock requested since it started may be its own: its
// duplicates' debris and background calls are held with it, and the hold grows
// with the traffic captured while it runs.
//
// It is a byte budget, not a count: a database mock (a result set) is 50 to
// 500 KiB where a small HTTP one is 2, and a count that fits the small ones
// let the large ones take hundreds of megabytes, with nothing else to stop
// them where the memory guard does not run.
//
// Past it, requests in flight are given up, the oldest first, among the
// managers holding more than their share (the budget over the number of
// managers holding anything): each time, the oldest of those managers' oldest
// requests (holdPool.oldestOverShare). If a given-up request is kept, its
// test case is left out, counted and warned of, rather than recorded without a
// mock (Window.Keep); a duplicate's costs nothing; what only it could own is
// let go. So no manager can make another give a request up while that one
// holds no more than its share: an app with a stream open under a flood of
// duplicates gives up its own requests, not the requests of the quiet apps
// beside it. A request claimed for its resolve (Claim, between it and
// ResolveKept) is never given up, and while a manager's oldest request is
// claimed none of its other requests is either (that one may own all it
// holds; its resolve takes what it owns).
//
// Where that happens: a reaper call (a resolve, a duplicate's prune, the
// periodic flush) that grows its manager's hold past the budget gives up the
// oldest such request itself, under its own lock, while that request is its
// manager's (fitHoldLocked); and once it has let go of its lock, those of the
// other managers, one manager's lock at a time (holdPool.settle). So the
// process holds more than the budget only while a reaper call that took it
// past is still running (by what that call held), or while the requests that
// could bring it back are claimed: whenever no reaper call is running, it is
// within it. TestProcessHoldNeverPassesItsBudget and
// TestProcessHoldStaysWithinItsBudgetUnderConcurrentApps drive many managers
// against it.
//
// So a request is recorded with its mocks while (its time in flight) x r stays
// within the room the budget leaves it, with r the bytes a second of mocks the
// reapers would drop for it, and is left out, counted and warned of, past
// that. With one app holding: 64 MiB, so with 2 KiB mocks an 11 s request up to
// about 3,000 such mocks a second, a 60 s one up to about 550, a 5 minute one
// up to about 110; with 100 KiB mocks, a fiftieth of those rates. With several
// apps holding at once, each keeps at least its share. The rate is of mocks
// the reapers drop: with static dedup, every duplicate's; without it, only the
// background calls no request made.
//
// Mocks are attributed by time alone, so this cannot tell a request whose
// early calls were let go from one that made none: a request given up is left
// out even if its calls all came late, where the in-flight tracker recorded
// it with them. Leaving it out, counted and warned of, is the choice the
// recording already makes for a test case whose window overlaps memory
// pressure or an unrecorded span, rather than keep a test case that may lack
// mocks.
//
// Memory: the hold takes at most this much of the process's heap, give or take
// the estimate (mockSize is within 0.92 to 1.16 of the heap a mock takes,
// TestMockSizeFollowsTheHeapAMockTakes) and what a reaper call holds before it
// gives requests up; whatever the number of managers
// (TestHoldKeepsWithinItsByteBudget, TestHoldKeepsWithinItsBudgetForAYoungRequest,
// TestHoldKeepsWithinItsBudgetAcrossManagers).
// Where the agent runs the memory guard (a Docker agent with a memory limit),
// memory pressure also empties the hold (SetMemoryPressure); elsewhere (a
// DaemonSet agent) the budget is the bound. CPU: an atomic store per request
// opened, claimed or ended, an atomic add per reaper call that changes the
// hold and a load to see the process within it; past it, a walk of the
// managers holding something per request given up (~0.25 us with 32 of them,
// BenchmarkPoolChoosesAmong32Apps).
const MaxHeldBytes int64 = 64 << 20

// holdPool is a hold budget shared by the managers that hold for requests in
// flight: what they hold (heldBytes), together. Every manager New (and Get)
// makes shares processHold; a manager built otherwise (a test's) gets a pool
// of its own of the same budget the first time it publishes (poolLocked).
//
// Lock order: shedMu, then a manager's mu, then mu. No manager's mu is ever
// taken under another's: a reaper call gives up only its own manager's
// requests under that manager's lock, and settle takes the others' one at a
// time, holding none. A reaper call only tries for shedMu under its manager's
// lock (TryLock), never waits on it there.
type holdPool struct {
	limit int64
	// total is the sum of what the members have published
	// (SyncMockManager.pooled).
	total atomic.Int64
	// mu guards members: the managers holding something, as of what they
	// published. A leaf: taken under a manager's mu, never the other way
	// around.
	mu      sync.Mutex
	members map[*SyncMockManager]struct{}
	// shedMu is held by whoever gives requests up for the pool (a settle, or
	// a reaper call giving up its own manager's under its lock), so two never
	// each give a request up for the same excess: each sees what the one
	// before freed.
	shedMu sync.Mutex
	// chose, if not nil (a test's pool), is told the manager a settle chose,
	// before the settle takes its lock: what happens to it in between is what
	// giveUpForPool checks again under that lock.
	chose func(*SyncMockManager)
	// afterOwnGiveUp, if not nil (a test's pool), is called by a reaper call
	// that held shedMu to give its own manager's requests up
	// (fitHoldLocked), the moment it lets go of shedMu and before anything
	// else it does, still under its manager's lock (unlockOwnShed): a settle
	// that takes shedMu there must see what those give-ups freed.
	afterOwnGiveUp func()
}

// unlockOwnShed lets go of shedMu for a reaper call that held it to give its
// own manager's requests up (fitHoldLocked), and then calls afterOwnGiveUp, if
// any: with nothing in between, so whatever the call must have published
// before a settle can take shedMu, it published before this.
func (p *holdPool) unlockOwnShed() {
	p.shedMu.Unlock()
	if p.afterOwnGiveUp != nil {
		p.afterOwnGiveUp()
	}
}

// processHold is the hold budget every manager of the process shares.
var processHold = newHoldPool(MaxHeldBytes)

func newHoldPool(limit int64) *holdPool {
	return &holdPool{limit: limit, members: make(map[*SyncMockManager]struct{})}
}

// poolLocked returns the pool m's hold counts against: processHold for a
// manager New or Get made, a pool of its own for one built otherwise. Caller
// holds mu.
func (m *SyncMockManager) poolLocked() *holdPool {
	if m.pool == nil {
		m.pool = newHoldPool(MaxHeldBytes)
	}
	return m.pool
}

// shareOfLocked is the most a member may hold without being made to give a
// request up for another's hold: the budget over the number of managers holding
// something, counting self (about to publish selfBytes) as one if it is not a
// member yet. Caller holds p.mu.
func (p *holdPool) shareOfLocked(self *SyncMockManager, selfBytes int64) int64 {
	k := int64(len(p.members))
	if self != nil && selfBytes > 0 {
		if _, in := p.members[self]; !in {
			k++
		}
	}
	if k == 0 {
		return p.limit
	}
	return p.limit / k
}

// oldestOverShare returns, of the managers holding more than their share, the
// one whose oldest request in flight is the oldest and not claimed, and when
// that request started (UnixNano): the next to give a request up for the
// pool. self, if not nil, is the caller's own manager, whose mu it holds: what
// it holds (selfBytes) and its oldest request (selfOldest, UnixNano, 0 for
// none or a claimed one) are given, since it has not published them yet. The
// others are read from what they published (pooledNow, oldestUnclaimed).
// passed managers are left out. nil when none can give one up.
func (p *holdPool) oldestOverShare(self *SyncMockManager, selfBytes, selfOldest int64, passed map[*SyncMockManager]bool) (*SyncMockManager, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	share := p.shareOfLocked(self, selfBytes)
	var best *SyncMockManager
	var bestAt int64
	if self != nil && selfBytes > share && selfOldest != 0 {
		best, bestAt = self, selfOldest
	}
	for c := range p.members {
		if c == self || passed[c] || c.pooledNow.Load() <= share {
			continue
		}
		if at := c.oldestUnclaimed.Load(); at != 0 && (best == nil || at < bestAt) {
			best, bestAt = c, at
		}
	}
	return best, bestAt
}

// publishHoldLocked brings what m has published to its pool up to its hold
// (heldBytes), and whether it is one of the pool's members. It runs where a
// change to the hold is complete — the end of a reaper call (fitHoldLocked),
// of a window's end or yield, of memory pressure, of a request given up for
// the pool — and not inside one, so the pool never counts a manager's hold
// mid-change. Caller holds mu.
func (m *SyncMockManager) publishHoldLocked() {
	p := m.poolLocked()
	if d := m.heldBytes - m.pooled; d != 0 {
		m.pooled = m.heldBytes
		m.pooledNow.Store(m.pooled)
		p.total.Add(d)
	}
	if in := m.pooled > 0; in != m.inPool {
		m.inPool = in
		p.mu.Lock()
		if in {
			p.members[m] = struct{}{}
		} else {
			delete(p.members, m)
		}
		p.mu.Unlock()
	}
}

// noteOldestLocked publishes when m's oldest request in flight started, for
// the pool to choose among managers without their locks (oldestOverShare): 0
// when none is in flight, or when the oldest is claimed (it is being resolved
// this moment, and no request after it can be given up for room: it may own
// everything held after its start). Runs wherever the open windows' first
// changes. Caller holds mu.
func (m *SyncMockManager) noteOldestLocked() {
	m.oldestUnclaimed.Store(m.oldestUnclaimedLocked())
}

// oldestUnclaimedLocked is when m's oldest request in flight started
// (UnixNano), 0 when none is or it is claimed. Caller holds mu.
func (m *SyncMockManager) oldestUnclaimedLocked() int64 {
	if w := m.earliestOpenLocked(); w != nil && !w.kept {
		return w.start.UnixNano()
	}
	return 0
}

// settlePool brings the pool m shares back within its budget, after a reaper
// call of m's that may have taken it past: with the requests of the other
// managers, which m's own lock could not take. Caller does not hold mu. One
// atomic load while the pool is within its budget.
func (m *SyncMockManager) settlePool() {
	if p := m.pool; p != nil && p.total.Load() > p.limit {
		p.settle()
	}
}

// settle gives requests up, the oldest first among the managers holding more
// than their share (oldestOverShare), one manager's lock at a time, until the
// pool is within its budget or none can be given up: a manager whose oldest
// request is claimed is passed over (its resolve takes what it owns, and runs
// a reaper call of its own), and so is one back within its share. One whose
// oldest request is no longer the one it was chosen for (that request ended
// meanwhile) is chosen among the rest again. It tells of them once it holds no
// lock.
func (p *holdPool) settle() {
	for _, g := range p.settleLocked() {
		g.report(p)
	}
}

func (p *holdPool) settleLocked() (gaveUp []poolGiveUp) {
	p.shedMu.Lock()
	defer p.shedMu.Unlock()
	var passed map[*SyncMockManager]bool
	pass := func(v *SyncMockManager) {
		if passed == nil {
			passed = make(map[*SyncMockManager]bool)
		}
		passed[v] = true
	}
	var stale map[*SyncMockManager]int64 // what each was chosen for, when that changed
	for p.total.Load() > p.limit {
		v, at := p.oldestOverShare(nil, 0, 0, passed)
		if v == nil {
			return gaveUp
		}
		if p.chose != nil {
			p.chose(v)
		}
		g, outcome := v.giveUpForPool(p, at)
		switch outcome {
		case poolWithin:
			return gaveUp
		case poolChanged:
			// Chosen again with what it published since. The same choice again
			// would say its published oldest request is not its own: pass it,
			// rather than choose it for ever.
			if was, ok := stale[v]; ok && was == at {
				pass(v)
				break
			}
			if stale == nil {
				stale = make(map[*SyncMockManager]int64)
			}
			stale[v] = at
		case poolPassed:
			pass(v)
		case poolGaveUp:
			gaveUp = append(gaveUp, g)
		}
	}
	return gaveUp
}

type poolOutcome int

const (
	poolGaveUp  poolOutcome = iota // a request was given up
	poolPassed                     // this manager cannot give one up now
	poolWithin                     // the pool is within its budget after all
	poolChanged                    // its oldest request is not the one chosen: choose again
)

// poolGiveUp is a request given up for the pool, with what its manager held
// before and its share then, and what its hold was left at: for the
// diagnostic its settle writes once it holds no lock.
type poolGiveUp struct {
	w             *Window
	before, share int64
	t             dropTally
	hold          holdState
}

// giveUpForPool gives up m's oldest request in flight for p, if p is still
// over its budget, m still holds more than its share, and that request is
// still the one it was chosen for (started at, UnixNano) and not claimed. Its
// verdict decides the cost: kept, it is left out (Window.Keep); a duplicate,
// nothing. Caller does not hold m.mu.
func (m *SyncMockManager) giveUpForPool(p *holdPool, at int64) (poolGiveUp, poolOutcome) {
	var g poolGiveUp
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.total.Load() <= p.limit {
		return g, poolWithin
	}
	p.mu.Lock()
	share := p.shareOfLocked(m, m.pooled)
	p.mu.Unlock()
	w := m.earliestOpenLocked()
	if m.pooled <= share || w == nil || w.kept {
		return g, poolPassed
	}
	if w.start.UnixNano() != at {
		return g, poolChanged
	}
	g.before, g.share = m.pooled, share
	w.givenUp, w.cause = true, LostToHoldBound
	m.endWindowLocked(w, &g.t, true)
	g.t.windowsGivenUp = 1
	m.publishHoldLocked()
	g.w, g.hold = w, m.holdStateLocked()
	return g, poolGaveUp
}

// report tells of a request given up for the pool: the sampled diagnostic of
// its manager (reportGivenUp), and what was let go with it. Caller holds no
// lock of the pool's or of a manager's.
func (g poolGiveUp) report(p *holdPool) {
	m := g.w.m
	m.reportGivenUp([]*Window{g.w}, LostToHoldBound)
	if ce := m.dropLogger().Check(zap.DebugLevel, "diag/syncMock: the process's hold passed its budget: gave up the oldest request in flight of the apps holding more than their share"); ce != nil {
		fields := append([]zap.Field{
			zap.Int64("hold_bound_bytes", p.limit),
			zap.Int64("app_held_bytes_before", g.before),
			zap.Int64("app_share_bytes", g.share),
		}, g.t.fields()...)
		ce.Write(append(fields, g.hold.fields()...)...)
	}
}
