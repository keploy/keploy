package async

import (
	"context"
	"sort"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// epoch is one value in a lane's timeline: the recorded response that becomes
// current at effectiveFromPos (a completed-count; 0 = initial/boot) and stays
// current until the next epoch.
type epoch struct {
	effectiveFromPos int
	seq              int
	mock             *models.Mock
}

type laneStream struct {
	lane   models.AsyncLane
	epochs []epoch // sorted by (effectiveFromPos, seq)
}

// currentEpoch returns the last epoch with effectiveFromPos <= completed, or
// nil if no epoch is effective yet.
func (s *laneStream) currentEpoch(completed int) *epoch {
	var cur *epoch
	for i := range s.epochs {
		if s.epochs[i].effectiveFromPos <= completed {
			cur = &s.epochs[i]
			continue
		}
		break // sorted ascending; nothing later qualifies
	}
	return cur
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
	streams    map[string]*laneStream // by lane name
	completed  int                    // number of testcases completed
	windowSeen bool                   // AdvanceWindow: first window doesn't count as a completed test

	// pass/flag are CUMULATIVE for this Engine's lifetime and are deliberately
	// never reset. CI parses the emitted verdict line with `tail -n1`
	// (.github/workflows/test_workflow_scripts/java/async_config_poll/java-linux.sh),
	// so the LAST line has to describe the whole run. Per-set deltas would
	// quietly narrow that gate from "no drift anywhere in the run" to "no drift
	// in the last chunk" — and worse, a poll woken by the test-set context
	// being cancelled (holdThrottle registers a context.AfterFunc that
	// broadcasts on cancel) lands AFTER the per-set flush, so the final line
	// would report served:0 on a run that served dozens.
	//
	// Scope, stated precisely: a straggler landing between the per-set flush
	// and the run-level one IS counted, because the run-level flush follows it.
	// One landing after the run-level flush is not — replay.go cancels the run
	// context immediately after that notify and no seam flushes afterwards
	// (Proxy.StopProxyServer would, but it is dead code with no call
	// expression anywhere). That tail was equally lost under the sync.Once
	// guard, so it is unchanged here rather than fixed.
	pass, flag int
	// flags holds drift details NOT YET EMITTED. LogReport drains it, so each
	// detail is logged exactly once even though LogReport now fires per
	// test-set. Draining is also what stops the output going quadratic: with
	// logOnce removed but the slice retained, every flush would re-log the
	// whole backlog.
	flags []string
	// lastPass/lastFlag are the tallies as of the last emitted line. LogReport
	// emits only when the cumulative counts have MOVED since then, which is
	// what keeps record mode silent (it never increments) and what suppresses a
	// duplicate identical line when the run-level flush follows a per-set flush
	// with no traffic in between.
	lastPass, lastFlag int

	// emitMu serialises drain-and-log as one unit. drainReport returns under
	// e.mu but the logger.Info call happens after it is released, so two
	// concurrent LogReport callers could otherwise write their lines in the
	// opposite order to their snapshots — and CI reads the LAST line, so it
	// would see the SMALLER total. The replayer happens to serialise the two
	// callers today; nothing in the code says so, and the whole CI contract
	// rests on the last line being the largest.
	//
	// Deliberately NOT pinned by a test: making the two callers interleave
	// deterministically means blocking one inside the other's critical section,
	// which deadlocks against e.mu, and a purely racing test scores no
	// detections under CI's `go test ./...`. This is defence in depth for a
	// path that is not reachable today, kept because the invariant it protects
	// is load-bearing and undocumented elsewhere — not because it is covered.
	emitMu sync.Mutex

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
			"test-anchored deliveries will report not-exercised — and a POLL delivery " +
			"anchored to a testcase will block its connection until replay teardown " +
			"(never arms) instead of keep-aliving — full async replay " +
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

// Load partitions async-tagged mocks into per-lane epoch timelines, sorted by
// (effectiveFromPos, seq). Each call REPLACES the previous corpus and rewinds
// the epoch cursor.
//
// It used to be run-once, which was wrong: one Engine serves a whole replay run
// (proxy.go builds it in InitIntegrations), while StoreMocks — the only thing
// that feeds it — runs once per TEST-SET. So sets 2..N had their corpus dropped
// and set 1's replayed in their place, and `completed`, which counts windows for
// the whole run while AnchorPos restarts at 0 in every set, made currentEpoch
// open mid-timeline so a set's boot value was never served.
//
// Replacing inside Load rather than on a separate reset seam is deliberate: the
// two replay branches call StoreMocks and MockOutgoing in OPPOSITE orders —
// compose is MockOutgoing then StoreMocks (replay.go:1239/1321), native is
// StoreMocks then MockOutgoing (replay.go:1420/1450) — so any seam in
// Proxy.Mock is correct for one path and wipes the just-loaded corpus on the
// other. Doing it here is ordering-independent.
//
// The report tallies (pass/flag/flags) are deliberately left alone; they are
// not per-set state.
func (e *Engine) Load(mocks []*models.Mock) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.streams = make(map[string]*laneStream)
	e.completed = 0
	e.windowSeen = false
	// A poll Decide parked on cond is waiting for a counter that just moved
	// backwards; wake it rather than let it block until its context expires.
	e.cond.Broadcast()
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
		s.epochs = append(s.epochs, epoch{
			effectiveFromPos: m.Spec.Async.AnchorPos,
			seq:              m.Spec.Async.Seq,
			mock:             m,
		})
	}
	for _, s := range e.streams {
		sort.SliceStable(s.epochs, func(i, j int) bool {
			if s.epochs[i].effectiveFromPos != s.epochs[j].effectiveFromPos {
				return s.epochs[i].effectiveFromPos < s.epochs[j].effectiveFromPos
			}
			return s.epochs[i].seq < s.epochs[j].seq
		})
	}
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
		// Guard the e.parsers read (RegisterParser writes it under e.mu), same
		// as Decide — so every e.parsers access is locked. MatchesLane runs
		// unlocked (it only reads the immutable lane + the request mock).
		e.mu.Lock()
		p := e.parsers[lane.BaseType()]
		e.mu.Unlock()
		if p != nil && p.MatchesLane(m, lane) {
			return lane, true
		}
	}
	return models.AsyncLane{}, false
}

// Decide returns the recorded response currently in effect for the lane — the
// last epoch with effectiveFromPos (Spec.Async.AnchorPos on disk) <= completed,
// re-selectable — or a keep-alive when no epoch is effective yet / the lane is
// unknown. It never blocks on an anchor.
//
// Scope note: this current-epoch SELECTION applies to EVERY async lane — poll
// AND non-poll — not only poll lanes. It replaces the old consume-once cursor
// (which served each recorded entry once, then keep-alived on drain). A non-poll
// lane therefore now re-serves its last-effective epoch on every request rather
// than walking a one-shot ordered sequence; sequential one-shot non-poll
// delivery is intentionally no longer supported. The only async lane that exists
// today (config-watch) is a value the client re-reads, for which current-epoch
// is the correct model — TestNonPollServesReselectableCurrentEpoch pins this so a
// future change can't regress it silently. Only the throttle hold below is gated
// to poll lanes; unchanged in scope from the pre-value-epoch engine is nothing.
func (e *Engine) Decide(ctx context.Context, lane models.AsyncLane, live *models.Mock) (*models.Mock, []byte, error) {
	e.mu.Lock()
	p := e.parsers[lane.BaseType()]
	s := e.streams[lane.Name]
	if s == nil || len(s.epochs) == 0 {
		e.mu.Unlock()
		return e.keepAlive(p, lane)
	}
	if lane.IsPoll() {
		e.holdThrottle(ctx, lane.ThrottleDuration()) // pace + wake-early; holds e.mu
	}
	// Re-read the stream: holdThrottle released e.mu, and a Load for the next
	// test-set may have replaced e.streams while this poll was parked. The
	// captured pointer would serve the PREVIOUS set's timeline against the
	// rewound counter — i.e. its boot value — instead of keep-aliving.
	s = e.streams[lane.Name]
	if s == nil || len(s.epochs) == 0 {
		e.mu.Unlock()
		return e.keepAlive(p, lane)
	}
	cur := s.currentEpoch(e.completed)
	e.mu.Unlock()
	if cur == nil {
		return e.keepAlive(p, lane)
	}
	return e.decideServe(p, lane, cur.mock, live)
}

// holdThrottle blocks up to d, or until an AdvanceWindow/OnTestComplete
// broadcast or ctx cancel, whichever comes first. Caller holds e.mu; it is
// released while parked and re-held on return.
//
// It paces EVERY poll — including the boot poll and a poll whose epoch already
// advanced — so the guarantee is "answered within at most d", NOT "spaced d
// apart". e.cond is shared across lanes and this is a single (non-looping) Wait,
// so any lane's timer or any AdvanceWindow/OnTestComplete wakes every parked
// poll; with several concurrent watch connections the per-poll pacing lower
// bound collapses (fine — one poll per connection is in flight). A spurious or
// early wake is harmless: Decide re-reads and serves the current epoch
// regardless. Wake-early-on-change only shortens the wait when an AdvanceWindow
// races the hold; a change that already happened before this poll arrived is
// still served on this poll within d.
func (e *Engine) holdThrottle(ctx context.Context, d time.Duration) {
	if ctx.Err() != nil {
		return
	}
	timer := time.AfterFunc(d, func() {
		e.mu.Lock()
		e.cond.Broadcast()
		e.mu.Unlock()
	})
	defer timer.Stop()
	stop := context.AfterFunc(ctx, func() {
		e.mu.Lock()
		e.cond.Broadcast()
		e.mu.Unlock()
	})
	defer stop()
	e.cond.Wait() // releases e.mu; wakes on timer, broadcast, or ctx
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

// decideServe runs the shape-match/verdict for the epoch Decide selected as
// current — after Decide's throttle hold (holdThrottle), if any, has elapsed
// or been woken early — and is about to be served. It takes no ctx: the match
// itself is synchronous and unlocked except for the pass/flag tally update, so
// it never serializes unrelated lanes' Load/Report/advance.
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

// Report snapshots verdict tallies. Under the epoch model every epoch is
// always serveable (re-selectable, never consumed), so there is no
// not-exercised or held count to compute.
func (e *Engine) Report() ReportSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.reportLocked()
	out.Flags = append(out.Flags, e.flags...)
	return out
}

// reportLocked builds the tally snapshot. Caller holds e.mu.
//
// Shared with drainReport so the two cannot drift: ReportSnapshot.Flags is
// populated only by Report, because drainReport hands the drift details back
// out-of-band after draining them. Anything that printed the verdict via
// Report would therefore re-log the whole backlog on every call — the
// quadratic-output bug the drain exists to prevent.
func (e *Engine) reportLocked() ReportSnapshot {
	return ReportSnapshot{Pass: e.pass, Flag: e.flag, NotExercised: 0, Held: 0}
}

// drainTestHook, when non-nil, runs between drainReport's snapshot and its
// cursor advance. It exists ONLY so a test can deterministically land an
// increment in that window: the window is otherwise unreachable by racing
// goroutines (Decide holds e.mu throughout unless the lane is a POLL lane, and
// even then the gap is a few instructions), so a purely concurrent test scores
// zero detections under CI's `go test ./...` — no -race, no -count>1. nil in
// production; the branch is one predictable-not-taken compare.
var drainTestHook func()

// drainReport returns the CUMULATIVE tallies together with the drift details
// not yet emitted, advances the emission cursor, and reports whether anything
// actually moved — all in ONE critical section.
//
// One section matters: MatchRequestShape runs unlocked and its increment takes
// e.mu afterwards, so a snapshot-then-separate-drain would let an increment
// land in the gap and be discarded without ever being logged.
//
// It deliberately does NOT delegate to Report(). sync.Mutex is not reentrant
// and Report() takes e.mu, so calling it from under the lock would self-
// deadlock the caller — which on the SetGracefulShutdown seam is the agent's
// HTTP handler goroutine. Handing e.flags over by assignment rather than
// copying is safe precisely because the engine drops its own reference in the
// same section.
func (e *Engine) drainReport() (ReportSnapshot, []string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pass == e.lastPass && e.flag == e.lastFlag {
		return ReportSnapshot{}, nil, false
	}
	snap := e.reportLocked()
	if drainTestHook != nil {
		drainTestHook()
	}
	drained := e.flags
	e.flags = nil
	// Advance the cursor to what was SNAPSHOT, not to the current values.
	// Advancing to the current values would mark as "reported" anything that
	// landed after the snapshot was taken — counted by the cursor, printed by
	// no line, and never picked up again because the cursor had moved past it.
	// e.mu makes that window empty in production (decideServe needs the lock to
	// increment), so this costs nothing and removes the dependence on that
	// being true.
	e.lastPass, e.lastFlag = snap.Pass, snap.Flag
	return snap, drained, true
}

// LogReport emits the async verdict tally so it is visible in keploy's output
// at end of replay: served-and-shape-matched (served) and shape-flags (served
// despite request drift). The not_exercised and held fields stay in the
// emitted log line for output-format stability (CI greps them) but are always
// 0 under the value-epoch model: every epoch is always serveable/re-
// selectable, so nothing arms-but-never-gets-exercised, and Decide no longer
// holds a lane's connection open — a poll lane is merely paced by throttle
// (holdThrottle), served lanes serve immediately. Each shape flag is logged at
// Info with a remediation hint.
func (e *Engine) LogReport(logger *zap.Logger) {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	s, drift, moved := e.drainReport()
	if !moved {
		return // nothing new since the last line (record mode, or a repeat flush)
	}
	// Field names and ORDER are load-bearing: java-linux.sh greps the encoded
	// line for `"served": N, "shape_flags": N, "not_exercised": N`. Renaming or
	// reordering these silently breaks that gate.
	logger.Info("async egress verdict",
		zap.Int("served", s.Pass),
		zap.Int("shape_flags", s.Flag),
		zap.Int("not_exercised", s.NotExercised),
		zap.Int("held", s.Held),
		// Appended AFTER held so the three fields CI greps stay contiguous.
		// The boundary flush runs inside RunTestSet, which the replayer has
		// already announced as the NEXT test-set, so without this a reader
		// attributes the previous set's verdict to the one just announced.
		zap.String("scope", "cumulative-for-run"))
	for _, f := range drift {
		logger.Info("async egress shape drift (served recorded response anyway); "+
			"re-record if the request legitimately changed, or widen the lane's "+
			"volatileParams / match to treat the varying part as noise",
			zap.String("detail", f))
	}
}
