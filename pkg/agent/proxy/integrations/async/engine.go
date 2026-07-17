package async

import (
	"context"
	"sort"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// asyncEntry is a recorded async mock with its ordering/anchor ints taken from
// the mock's Async block at Load, so the hot path never re-reads them.
type asyncEntry struct {
	mock      *models.Mock
	seq       int
	anchorPos int
	poll      bool // Async.Poll: HOLD this delivery at Decide until anchorPos is reached
}

type laneStream struct {
	lane   models.AsyncLane
	mocks  []asyncEntry // sorted by seq
	cursor int          // next unconsumed index
}

// ReportSnapshot is a point-in-time copy of the engine's verdict tallies.
type ReportSnapshot struct {
	Pass         int
	Flag         int
	NotExercised int
	Held         int
	Flags        []string
}

// Engine is transport-agnostic: it holds only *models.Mock, models.AsyncLane,
// and AsyncParser delegates. Safe for concurrent use.
type Engine struct {
	logger    *zap.Logger
	laneOrder []models.AsyncLane     // caller-declared order; first match wins in LaneFor, by-name lookup scans it
	parsers   map[string]AsyncParser // by lane.Type
	hasPoll   bool                   // true if any configured lane is a poll lane; lets the record path skip poll detection when none are

	mu         sync.Mutex
	cond       *sync.Cond             // signaled whenever completed/windowSeen changes; wakes held POLL Decide calls
	loaded     bool                   // true once Load has partitioned+sorted streams; makes Load idempotent
	streams    map[string]*laneStream // by lane name
	completed  int                    // number of testcases completed
	windowSeen bool                   // AdvanceWindow: first window doesn't count as a completed test

	pass, flag, held int
	flags            []string

	logOnce         sync.Once // LogReport fires at most once (multiple shutdown seams call it)
	nonWindowedOnce sync.Once // WarnNonWindowed fires at most once (SetMocks runs per test-set)
}

func NewEngine(logger *zap.Logger, lanes []models.AsyncLane, parsers map[string]AsyncParser) *Engine {
	// WithEffectiveNames both fills omitted names (matching what the recorder
	// stamped) and returns its own copy, so laneOrder is safe from caller
	// mutation.
	laneOrder := models.WithEffectiveNames(lanes)
	// Surface each lane's resolved match criteria once at startup. An
	// over-broad match (e.g. path "/*" or a host glob that covers a real
	// dependency) silently captures ordinary sync egress: once that lane's
	// recorded stream drains, every such request gets an empty keep-alive
	// instead of its real recorded response. Logging the resolved criteria
	// makes an over-broad glob visible; keep a lane's match as narrow as the
	// poll endpoint.
	for _, l := range laneOrder {
		logger.Info("async egress lane configured",
			zap.String("lane", l.Name),
			zap.String("type", l.Type),
			zap.Any("match", l.Match),
			zap.Any("matchQuery", l.MatchQuery))
	}
	hasPoll := false
	for _, l := range laneOrder {
		if l.IsPoll() {
			hasPoll = true
			break
		}
	}
	e := &Engine{
		logger:    logger,
		laneOrder: laneOrder,
		parsers:   parsers,
		hasPoll:   hasPoll,
		streams:   make(map[string]*laneStream),
	}
	e.cond = sync.NewCond(&e.mu)
	return e
}

// HasPollLanes reports whether any configured lane is a poll lane. The record
// path uses it to skip poll detection (and its request re-parse) entirely when
// no poll lanes are configured.
func (e *Engine) HasPollLanes() bool { return e.hasPoll }

// WarnNonWindowed logs once that async replay is running on the non-windowed
// mock path (Proxy.SetMocks, the fallback used when the proxy does not
// implement WindowedProxy). That path never calls AdvanceWindow, so the
// completed counter stays at 0 and only startup-anchored (anchorPos == 0)
// deliveries ever arm — every test-anchored delivery would otherwise report
// not-exercised with no indication that gating never advanced. Full async
// replay requires the windowed mock manager.
func (e *Engine) WarnNonWindowed() {
	e.nonWindowedOnce.Do(func() {
		e.logger.Warn("async egress lanes are configured but replay is using the " +
			"non-windowed mock path; only startup-anchored deliveries will arm and " +
			"test-anchored deliveries will report not-exercised — full async replay " +
			"requires the windowed mock manager (WindowedProxy)")
	})
}

// laneByName finds a declared lane by name. Lane counts are tiny (caller-
// declared config), so a linear scan is cheaper than a parallel map.
func (e *Engine) laneByName(name string) (models.AsyncLane, bool) {
	for _, l := range e.laneOrder {
		if l.Name == name {
			return l, true
		}
	}
	return models.AsyncLane{}, false
}

// Load partitions async-tagged mocks into per-lane, seq-ordered streams.
// It is run-once: the first call partitions and sorts; subsequent calls are
// no-ops so a re-Load after Decide has advanced a cursor cannot re-serve an
// already-consumed mock.
func (e *Engine) Load(mocks []*models.Mock) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loaded {
		return
	}
	for _, m := range mocks {
		if m == nil || !m.IsAsync() {
			continue
		}
		name := m.Spec.Async.Lane
		lane, ok := e.laneByName(name)
		if !ok {
			continue
		}
		s := e.streams[name]
		if s == nil {
			s = &laneStream{lane: lane}
			e.streams[name] = s
		}
		s.mocks = append(s.mocks, asyncEntry{
			mock:      m,
			seq:       m.Spec.Async.Seq,
			anchorPos: m.Spec.Async.AnchorPos,
			poll:      m.Spec.Async.Poll,
		})
	}
	for _, s := range e.streams {
		sort.SliceStable(s.mocks, func(i, j int) bool {
			return s.mocks[i].seq < s.mocks[j].seq
		})
	}
	e.loaded = true
}

// OnTestComplete increments the completed-testcase counter directly. Used by
// unit tests to simulate test completion; production advances via AdvanceWindow.
func (e *Engine) OnTestComplete() {
	e.mu.Lock()
	e.completed++
	e.cond.Broadcast()
	e.mu.Unlock()
}

// AdvanceWindow records that the replay runner opened a test window. The first
// window is the app reaching its first test (no test completed yet); every
// subsequent window means one more test completed. Owning the "first window
// doesn't count" rule here lets callers advance unconditionally.
func (e *Engine) AdvanceWindow() {
	e.mu.Lock()
	if e.windowSeen {
		e.completed++
	} else {
		e.windowSeen = true
	}
	e.cond.Broadcast()
	e.mu.Unlock()
}

// RegisterParser attaches a parser for a lane type. Called only during
// Proxy.InitIntegrations (startup, before any serving).
func (e *Engine) RegisterParser(t string, p AsyncParser) {
	e.mu.Lock()
	if e.parsers == nil {
		e.parsers = map[string]AsyncParser{}
	}
	e.parsers[t] = p
	e.mu.Unlock()
}

// CompletedForTest exposes the completed-testcase counter for wiring tests.
func (e *Engine) CompletedForTest() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.completed
}

// LaneFor returns the lane a live request routes to, by asking each lane's
// parser. Lanes are tried in caller-declared order (as passed to NewEngine)
// so the FIRST declared matching lane always wins, deterministically.
func (e *Engine) LaneFor(m *models.Mock) (models.AsyncLane, bool) {
	for _, lane := range e.laneOrder {
		p := e.parsers[lane.BaseType()]
		if p != nil && p.MatchesLane(m, lane) {
			return lane, true
		}
	}
	return models.AsyncLane{}, false
}

// Decide returns the recorded mock to serve, or a keep-alive payload.
//
// A POLL delivery (asyncEntry.poll, from the mock's Async.Poll) is HELD
// here on e.cond until completed reaches its anchorPos or ctx is done.
// cond.Wait releases e.mu while parked, so AdvanceWindow/OnTestComplete are
// NEVER gated by an outstanding held poll — that is the non-blocking
// invariant this method must preserve. A non-poll delivery that isn't armed
// yet returns a keep-alive immediately (unchanged pre-hold behavior). The
// (potentially expensive) shape match itself still runs OUTSIDE the engine
// lock, in decideServe, so poll traffic on one lane never serializes
// unrelated lanes' Load/Report/advance.
//
// The hold is a re-peek LOOP, not a single wait-then-serve: under concurrent
// same-lane Decide calls, another consumer can advance s.cursor while this
// call is parked in cond.Wait. Every wake (and the initial pass) re-reads the
// CURRENT s.mocks[s.cursor] and re-checks its own anchorPos before serving —
// serving a stale, pre-wait entry could arm a later, not-yet-armed delivery.
func (e *Engine) Decide(ctx context.Context, lane models.AsyncLane, live *models.Mock) (*models.Mock, []byte, error) {
	e.mu.Lock()
	p := e.parsers[lane.BaseType()]
	// A ctx-done bridge wakes a parked cond.Wait below on cancellation
	// (normal progress is covered by AdvanceWindow/OnTestComplete broadcasts).
	// It is only needed once we actually park, so it is registered lazily
	// before the first Wait — the immediate-return paths that dominate
	// non-poll async traffic never pay for it. stopBridge is idempotent and
	// safe to call after the callback has run.
	var stopBridge func() bool // context.AfterFunc's stop; nil until we park
	defer func() {
		if stopBridge != nil {
			stopBridge()
		}
	}()

	for {
		s := e.streams[lane.Name]
		if s == nil || s.cursor >= len(s.mocks) {
			e.mu.Unlock()
			return e.keepAlive(p, lane) // exhausted / unknown lane
		}
		entry := s.mocks[s.cursor]
		if e.completed >= entry.anchorPos {
			if entry.poll {
				e.held++
			}
			recorded := entry.mock
			s.cursor++
			e.mu.Unlock()
			return e.decideServe(p, lane, recorded, live)
		}
		if !entry.poll {
			// non-poll, not armed: immediate keep-alive (unchanged behavior).
			e.mu.Unlock()
			return e.keepAlive(p, lane)
		}
		if ctx.Err() != nil {
			e.mu.Unlock()
			return e.keepAlive(p, lane)
		}
		if stopBridge == nil {
			stopBridge = context.AfterFunc(ctx, func() {
				e.mu.Lock()
				e.cond.Broadcast()
				e.mu.Unlock()
			})
		}
		e.cond.Wait() // releases e.mu; loop re-peeks the (possibly advanced) cursor on wake
	}
}

// keepAlive returns the parser's "no data yet" payload for lane. Nil-safe:
// when no parser is registered for the lane's base type, it returns a bare
// (nil, nil, nil) rather than panicking.
func (e *Engine) keepAlive(p AsyncParser, lane models.AsyncLane) (*models.Mock, []byte, error) {
	if p == nil {
		return nil, nil, nil
	}
	ka, err := p.EmptyResponse(lane)
	return nil, ka, err
}

// decideServe runs the shape-match/verdict for a recorded delivery that has
// been armed (and, for polls, already released from its hold) and is about to
// be served. ctx is accepted for symmetry with Decide/future cancellation-
// aware matching; the match itself is synchronous today.
func (e *Engine) decideServe(p AsyncParser, lane models.AsyncLane, recorded, live *models.Mock) (*models.Mock, []byte, error) {
	if p == nil {
		if e.logger != nil {
			e.logger.Debug("async: no parser registered for lane type; serving recorded mock unverified — "+
				"check async.lanes[].type matches a registered integration that implements AsyncParser",
				zap.String("lane", lane.Name), zap.String("type", lane.Type))
		}
		return recorded, nil, nil // serve recorded even without a shape verdict
	}

	ok, detail := p.MatchRequestShape(live, recorded, lane) // unlocked — may parse/compare
	e.mu.Lock()
	if ok {
		e.pass++
	} else {
		e.flag++
		e.flags = append(e.flags, lane.Name+": "+detail)
	}
	e.mu.Unlock()
	return recorded, nil, nil // serve recorded either way
}

// Report snapshots verdict tallies, counting undrained armed mocks as not-exercised.
func (e *Engine) Report() ReportSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	ne := 0
	for _, s := range e.streams {
		for i := s.cursor; i < len(s.mocks); i++ {
			if e.completed >= s.mocks[i].anchorPos {
				ne++
			}
		}
	}
	out := ReportSnapshot{Pass: e.pass, Flag: e.flag, NotExercised: ne, Held: e.held}
	out.Flags = append(out.Flags, e.flags...)
	return out
}

// LogReport emits the async verdict tally so it is visible in keploy's output
// at end of replay: served-and-shape-matched, shape-flags (served despite
// request drift), and armed-but-never-polled (not-exercised). Each shape flag
// is logged at Info with a remediation hint.
func (e *Engine) LogReport(logger *zap.Logger) {
	e.logOnce.Do(func() {
		s := e.Report()
		if s.Pass == 0 && s.Flag == 0 && s.NotExercised == 0 {
			return // no async activity (e.g. record mode) — stay quiet
		}
		logger.Info("async egress verdict",
			zap.Int("served", s.Pass),
			zap.Int("shape_flags", s.Flag),
			zap.Int("not_exercised", s.NotExercised),
			zap.Int("held", s.Held))
		for _, f := range s.Flags {
			logger.Info("async egress shape drift (served recorded response anyway); "+
				"re-record if the request legitimately changed, or widen the lane's "+
				"volatileParams / match to treat the varying part as noise",
				zap.String("detail", f))
		}
	})
}
