package http

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// Record's full-round exact-match fast path (learnMatchIn).
//
// In record's full round (opts.Learn under KEPLOY_PROVEN=1), every call is
// decided by learnMatchIn. Without an index, each call took a full
// RoundSnapshot (copying every recording and tracker slice), sorted all
// recordings in recorded order, built learn.Shape (decoding JSON bodies) for
// every candidate of the same method+segment-count, and scanned them in
// learn.Pair. For a set of n recordings, per-call cost grew O(n) and a full
// round took O(n^2).
//
// By Theorem (learn.Pair exact-match equivalence): the first unspent,
// non-background candidate (in scope order: own recordings in recorded order,
// then pool recordings in recorded order) that matches liveShape with zero
// structural, drift, or reorder differences is ALWAYS the exact winner
// learn.Pair returns, regardless of all spent, background, non-exact, or later
// candidates.
//
// This fast path shares HTTP.learnIx (ownedIndexes[*learnOwned],
// genIndexes[*learnIndex] for shared and pool views) and learnPlainPrint /
// learnLivePrint (learn_body.go) with replay's learnedMatchOf so there is ONE
// index family and ONE request-print derivation in memory. Monotonic CAS
// cursors on owned and pool buckets skip permanently consumed/spent prefixes
// in O(1) amortised; sharedIx never advances cursors across views. Exact-match
// calls resolve without taking a full RoundSnapshot; drift, misses,
// declared-noise rewrites, store read-backs, background-only matches,
// LearnAwaiting, meta.Ahead pool views, and boundary diagnostics fall back to
// the full readLearnPass + learn.Pair path.

var (
	fullRoundFastHits         atomic.Int64
	fullRoundVisits           atomic.Int64
	fullRoundRetakePoolBuilds atomic.Int64
	fullRoundVerifySkips      atomic.Int64
)

type fullRoundStep uint8

const (
	fullRoundFallback fullRoundStep = iota
	fullRoundServed
	fullRoundMiss
	fullRoundRetake
)

type fullRoundOutcome struct {
	step      fullRoundStep
	out       *models.Mock
	miss      *learnMiss
	callRound integrations.CallRound
	setCall   bool
}

// learnLoopState holds learnMatchIn's per-call loop state shared between the
// fast path (tryExactLearnMatch) and the full path (readLearnPass).
type learnLoopState struct {
	marked     bool
	made       learnRound
	madePool   []*models.Mock
	madePoolFn func() []*models.Mock
	alone      string
	retake     bool
	unclaimed  map[*models.Mock]learnPass
}

func needPoolGroup(m *models.Mock, opts models.OutgoingOptions, verifying bool) bool {
	return verifying || (!opts.DisableStatefulMocks && m != nil && m.TestModeInfo.Consume == models.ConsumeCursorSaturate)
}

// isAnswered reports whether name is already answered for the open test — either
// in tracker.Answered or as a staging TakeVoided entry held by the open view.
func isAnswered(tracker *learn.Tracker, meta integrations.RoundMeta, name string) bool {
	return (tracker != nil && tracker.Answered(name)) || (meta.Open && meta.IsVoided(name))
}

// hasMatchingPut reports whether tracker has served any HTTP PUT mock in the
// running test whose resource target equals read's.
func hasMatchingPut(tracker *learn.Tracker, read *models.Mock) bool {
	if tracker == nil {
		return false
	}
	puts := tracker.HTTPPuts()
	if len(puts) == 0 {
		return false
	}
	t, ok := putTarget(read)
	if !ok {
		return false
	}
	for _, p := range puts {
		if pt, pok := putTarget(p); pok && pt == t {
			return true
		}
	}
	return false
}

// tryExactLearnMatch attempts to resolve a single pass of learnMatchIn via
// O(1) RoundMeta + HTTP.learnIx. It serves ONLY when it proves an unspent,
// non-background exact match with no declared-noise rewrite and no pending
// read-back write; every other case returns fullRoundFallback so learnMatchIn
// runs the full readLearnPass + learn.Pair path.
func (h *HTTP) tryExactLearnMatch(
	ctx context.Context,
	input *req,
	mockDb integrations.MockMemDb,
	tracker *learn.Tracker,
	live httpParts,
	liveShape learn.Shape,
	n learnNoise,
	opts models.OutgoingOptions,
	mark integrations.CallRound,
	wantCall bool,
	st *learnLoopState,
) (fullRoundOutcome, error) {
	rev := storeRevision(mockDb)
	meta, ok := integrations.ReadRoundMeta(mockDb, tracker)
	if !ok {
		return fullRoundOutcome{step: fullRoundFallback}, nil
	}
	nowPass := learnPass{rev: rev, round: meta.At}
	nowRound := learnRound{
		read:    true,
		open:    meta.Learn.Open,
		test:    meta.Learn.Number,
		snap:    true,
		agrees:  meta.Phase == integrations.RoundInTest || meta.Phase == integrations.RoundBetween,
		epoch:   meta.At.Epoch,
		viewGen: meta.ViewGen(),
		view:    meta.Test,
		noOne:   meta.NoOneRound(),
		ahead:   meta.TrackerAhead(),
		alone:   meta.ViewAlone(),
	}
	if !st.made.read && mark.MovedSince(nowPass.round) {
		res := learn.Result{Outcome: learn.Miss, Index: -1, Why: markMovedWhy(mark, nowRound.snap, nowPass.round)}
		return fullRoundOutcome{step: fullRoundMiss, miss: &learnMiss{res: res, moved: true}}, nil
	}
	moved, caughtUp := st.made.movedFrom(nowRound, st.retake)
	if moved != "" || nowRound.noOne != "" {
		// Rare moved-round or boundary miss: fall back to full readLearnPass
		// so the miss carries the exact HTTP pool diagnostic.
		return fullRoundOutcome{step: fullRoundFallback}, nil
	}
	nextMade := st.made
	if !nextMade.read || caughtUp {
		nextMade = nowRound
	}
	if (nextMade.waits() && nowRound.ahead) || meta.Learn.Awaiting {
		// Rare boundary wait or request-awaiting pause: fall back to
		// readLearnPass so retake reset and madePool stay identical.
		return fullRoundOutcome{step: fullRoundFallback}, nil
	}
	commitRound := func(poolFn func() []*models.Mock) (integrations.CallRound, bool) {
		var cr integrations.CallRound
		var setCall bool
		if wantCall && (!st.marked || caughtUp) {
			cr, st.marked, setCall = meta.CallRound(), true, true
		}
		if !st.made.read {
			st.alone = nowRound.alone
		}
		if !st.made.read || caughtUp {
			st.made = nowRound
			st.madePool = nil
			st.madePoolFn = poolFn
		}
		return cr, setCall
	}

	stuck := func(m *models.Mock) bool {
		at, exists := st.unclaimed[m]
		return exists && at == nowPass
	}

	urlKey := urlNoiseKey(n.url)
	liveMasked := n.maskedPath(live.path)
	pathKey := live.method + " " + liveMasked
	wildKey := live.method + " " + strconv.Itoa(segmentCount(liveMasked))
	livePrint, hasLivePrint := n.learnLivePrint(live)

	var (
		firstMatch  *models.Mock
		firstRec    learn.Shape
		firstRP     httpParts
		firstOwn    bool
		hasDeclared bool
		ownSeen     map[*models.Mock]bool
		poolGroup   []*models.Mock
		viewFlatIx  *learnIndex
	)

	// Case A: open test view (meta.Test != "").
	if meta.Test != "" {
		sr, ok := mockDb.(integrations.SharedReads)
		if !ok {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		ownIx, ownView, _, ok := h.learnIx.owned.of(mockDb, sr, urlKey, func(owned []*models.Mock) *learnOwned {
			return buildLearnOwned(owned, n)
		})
		if !ok || ownIx == nil || ownView != meta.OwnedGen {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		var sharedPT, sharedS []*models.Mock
		sharedIx, sharedKey, ok := h.learnIx.shared.sharedOf(mockDb, mockDb, urlKey, func() error {
			var sok bool
			if sharedPT, sharedS, sok = sr.SharedMocks(); !sok {
				return errNoSharedReads
			}
			return nil
		}, func(key learnIndexKey) *learnIndex {
			return buildLearnIndex(key, sharedPT, sharedS, n)
		})
		if !ok || sharedIx == nil || sharedKey.shared != meta.SharedGen {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		viewFlatIx = sharedIx
		ownRanks := ownIx.byPath[pathKey]
		ownWild := ownIx.wild[wildKey]
		sharedEntries := sharedIx.byPath[pathKey]
		sharedWild := sharedIx.wild[wildKey]

		// Fast path A1: open test agreeing with tracker, no matching shared
		// entries, no matching wild entries in owned -> scan owned bucket
		// directly in O(1) amortised without allocating candidate lists.
		if meta.Open && len(sharedEntries) == 0 && len(sharedWild) == 0 && len(ownWild) == 0 {
			if len(ownRanks) == 0 {
				return fullRoundOutcome{step: fullRoundFallback}, nil
			}
			m, recShape, rp, decl, matched, err := h.matchOwnedDirect(ctx, ownIx, pathKey, ownRanks, meta, tracker, live, liveShape, n, livePrint, hasLivePrint)
			if err != nil {
				return fullRoundOutcome{}, err
			}
			if !matched {
				return fullRoundOutcome{step: fullRoundFallback}, nil
			}
			firstMatch, firstRec, firstRP, firstOwn, hasDeclared = m, recShape, rp, true, decl
		} else {
			// General open-test case (shared entries, wild entries, or !meta.Open).
			m, recShape, rp, own, decl, seen, pg, matched, err := h.matchOpenTestMerged(
				ctx, ownIx, pathKey, ownRanks, ownWild, sharedIx, sharedEntries, sharedWild,
				meta, mockDb, tracker, live, liveShape, n, opts, livePrint, hasLivePrint,
			)
			if err != nil {
				return fullRoundOutcome{}, err
			}
			if !matched {
				return fullRoundOutcome{step: fullRoundFallback}, nil
			}
			firstMatch, firstRec, firstRP, firstOwn, hasDeclared, ownSeen, poolGroup = m, recShape, rp, own, decl, seen, pg
		}
	} else {
		// Case B: pool view (meta.Test == "").
		if meta.Ahead {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		var poolPT, poolS []*models.Mock
		poolIx, ok := h.learnIx.genIndexes.of(mockDb, mockDb, urlKey, func() error {
			var err error
			if poolPT, err = mockDb.GetPerTestMocksInWindow(); err != nil {
				return err
			}
			//nolint:staticcheck // SA1019: matches readLearnPass/RoundSnapshot session pool union.
			poolS, err = mockDb.GetSessionMocks()
			return err
		}, func(key learnIndexKey) *learnIndex {
			return buildLearnIndex(key, poolPT, poolS, n)
		})
		if !ok || poolIx == nil || poolIx.gen != meta.StagingGen {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		viewFlatIx = poolIx
		poolEntries := poolIx.byPath[pathKey]
		poolWild := poolIx.wild[wildKey]
		if len(poolEntries) == 0 && len(poolWild) == 0 {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		m, recShape, rp, decl, pg, matched, err := h.matchPoolView(
			ctx, poolIx, pathKey, poolEntries, poolWild, meta, mockDb, live, liveShape, n, opts, livePrint, hasLivePrint,
		)
		if err != nil {
			return fullRoundOutcome{}, err
		}
		if !matched {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		firstMatch, firstRec, firstRP, firstOwn, hasDeclared, poolGroup = m, recShape, rp, false, decl, pg
	}

	// Verify mode cross-check: when ExactViewVerify is on, check our fast-path
	// winner against a full reference readLearnPass + learn.Pair at the same
	// round instant (snap.At == meta.At).
	if h.verifying() {
		if snap, ok := integrations.SnapshotRound(mockDb, integrations.RoundQuery{Tracker: tracker}); ok && snap.At == meta.At {
			// Skip check if a concurrent call answered firstMatch between our
			// lookup and SnapshotRound (tracker.Answer does not bump meta.At).
			if !firstOwn || tracker == nil || !tracker.Answered(firstMatch.Name) {
				refSc := h.learnScopeIn(snap, live, liveShape, n, opts.LearnPool)
				refRes := learn.Pair(liveShape, refSc.cands)
				if refRes.Outcome != learn.Exact || refSc.mocks[refRes.Index] != firstMatch || refSc.own[refRes.Index] != firstOwn {
					var refName string
					var refOwn bool
					if refRes.Index >= 0 && refRes.Index < len(refSc.mocks) {
						refName = refSc.mocks[refRes.Index].Name
						refOwn = refSc.own[refRes.Index]
					}
					panic(fmt.Sprintf("exact view verify: full-round exact match of %s %s: fast (%s, own=%v), full (outcome=%v, %s, own=%v)",
						live.method, live.path, firstMatch.Name, firstOwn, refRes.Outcome, refName, refOwn))
				}
				if !firstOwn {
					refGroup := exactGroup(liveShape, refSc, true)
					if !slices.Equal(poolGroup, refGroup) {
						panic(fmt.Sprintf("exact view verify: full-round pool group of %s %s: fast %v, full %v",
							live.method, live.path, strictNames(poolGroup), strictNames(refGroup)))
					}
				}
			} else {
				fullRoundVerifySkips.Add(1)
			}
		} else {
			fullRoundVerifySkips.Add(1)
		}
	}

	// Pick via cursorPick for pool matches, identical to learnMatchIn.
	picked, rec, rp, commit := firstMatch, firstRec, firstRP, func() {}
	if !firstOwn {
		if ownSeen[firstMatch] {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		var c func()
		var answered *answeredSequence
		picked, c, answered = h.cursorPick(firstMatch, poolGroup, func(m *models.Mock) bool { return ownSeen[m] }, mockDb, !opts.DisableStatefulMocks)
		if answered != nil {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		if c != nil {
			commit = c
		}
		if picked != firstMatch {
			if ownSeen[picked] {
				return fullRoundOutcome{step: fullRoundFallback}, nil
			}
			rp = recordedParts(picked)
			rec = n.shape(rp, destOf(live, rp))
			hasDeclared = len(learn.Compare(liveShape, rec, n.known(picked)).Declared) > 0
		}
	}

	if stuck(picked) {
		return fullRoundOutcome{step: fullRoundFallback}, nil
	}

	// Store read-back check: fall back only when a registered store reader
	// reads picked or a matching HTTP PUT was actually served in this test.
	if firstOwn && (models.HTTPStoreReads(picked.Spec.HTTPReq) || (meta.LearnHasHTTPPut && putReads(picked) && hasMatchingPut(tracker, picked))) {
		return fullRoundOutcome{step: fullRoundFallback}, nil
	}

	if input.written != nil {
		ans, err := withResponse(picked)
		if err != nil {
			return fullRoundOutcome{}, err
		}
		if input.written(ans) == nil {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
	}

	// Declared-noise drift requires classify + declaredPlan: fall back.
	if hasDeclared {
		return fullRoundOutcome{step: fullRoundFallback}, nil
	}

	if firstOwn && !tracker.Answer(meta.Learn.Number, picked) {
		cr, setCall := commitRound(func() []*models.Mock {
			return h.buildRetakePool(mockDb, tracker, meta, viewFlatIx, live, liveShape, n, opts.LearnPool)
		})
		return fullRoundOutcome{step: fullRoundRetake, callRound: cr, setCall: setCall}, nil
	}

	claimDb := integrations.WithinRound(mockDb, meta.At)
	ownerFn := func(m *models.Mock) bool { return m == picked && firstOwn }
	out, claimed, err := h.claim(context.WithValue(ctx, ownedKey{}, ownerFn), picked, claimDb, nil, nil)
	if err != nil || !claimed {
		if firstOwn {
			tracker.Unanswer(meta.Learn.Number, picked.Name)
		}
		if err != nil {
			return fullRoundOutcome{}, err
		}
		st.unclaimed[picked] = nowPass
		cr, setCall := commitRound(func() []*models.Mock {
			return h.buildRetakePool(mockDb, tracker, meta, viewFlatIx, live, liveShape, n, opts.LearnPool)
		})
		return fullRoundOutcome{step: fullRoundRetake, callRound: cr, setCall: setCall}, nil
	}
	cr, setCall := commitRound(nil)
	commit()
	if e, ok := n.servedExchange(tracker.Drifts(), picked, nil, rec, rp, liveShape, input, live, out, firstOwn, answerView{}); ok {
		tracker.Served(meta.Learn.Number, learn.Named(e, nil, nil))
	}
	fullRoundFastHits.Add(1)
	return fullRoundOutcome{step: fullRoundServed, out: out, callRound: cr, setCall: setCall}, nil
}

// buildRetakePool materialises pass 1's diagnostic pool (matching read.sc.http
// of readLearnPass / learnScopeIn) lazily ONLY when learnMatchIn actually
// returns a moved-round miss after a pass-1 claim or tracker.Answer refusal.
// Contended refusals that retry and succeed in the same round never call this,
// keeping identical-request bursts O(1).
//
// Once pass 1's view is replaced by a newer round, its consumed/spent state is
// frozen, so deferred materialisation yields the same pool as building at
// refusal time. Under concurrent consumes while pass 1's view is still active,
// materialisation reads the view at materialisation time rather than pass 1's
// initial read (interleaving-dependent in both paths).
func (h *HTTP) buildRetakePool(
	mockDb integrations.MockMemDb,
	tracker *learn.Tracker,
	meta integrations.RoundMeta,
	viewFlatIx *learnIndex,
	live httpParts,
	liveShape learn.Shape,
	n learnNoise,
	learnPool bool,
) []*models.Mock {
	fullRoundRetakePoolBuilds.Add(1)
	// When the store is still at meta.At, ask the full path's RoundSnapshot
	// + learnScopeIn directly so the pool is identical by construction.
	if snap, ok := integrations.SnapshotRound(mockDb, integrations.RoundQuery{Tracker: tracker}); ok && snap.At == meta.At {
		return h.learnScopeIn(snap, live, liveShape, n, learnPool).http
	}
	// The round already moved under h.claim / tracker.Answer: reconstruct
	// pass 1's perTest and session tiers (including recordings with variables
	// and excluding consumed/spent ones) from meta's captured view and
	// viewFlatIx, then sort identically to scopeReads.scope.
	var ownedUnspent []*models.Mock
	if meta.Test != "" && len(meta.Owned) > 0 {
		for from := 0; from < len(meta.Owned); {
			idx, _, ok := meta.UnspentFrom(nil, nil, from)
			if !ok {
				break
			}
			ownedUnspent = append(ownedUnspent, meta.Owned[idx])
			from = idx + 1
		}
	}
	spent := meta.IsSpent
	var perTest, session []*models.Mock
	if meta.Test != "" {
		perTest = append(perTest, ownedUnspent...)
		if viewFlatIx != nil {
			for _, e := range viewFlatIx.entries[:viewFlatIx.split] {
				if !spent(e.m.Name) {
					perTest = append(perTest, e.m)
				}
			}
			for _, m := range viewFlatIx.varPerTest {
				if !spent(m.Name) {
					perTest = append(perTest, m)
				}
			}
		}
		seen := make(map[*models.Mock]struct{}, len(ownedUnspent))
		for _, m := range ownedUnspent {
			if m != nil {
				if _, dup := seen[m]; !dup {
					seen[m] = struct{}{}
					session = append(session, m)
				}
			}
		}
		if viewFlatIx != nil {
			for _, e := range viewFlatIx.entries[viewFlatIx.split:] {
				if e.m != nil && !spent(e.m.Name) {
					if _, dup := seen[e.m]; !dup {
						seen[e.m] = struct{}{}
						session = append(session, e.m)
					}
				}
			}
			for _, m := range viewFlatIx.varSession {
				if m != nil && !spent(m.Name) {
					if _, dup := seen[m]; !dup {
						seen[m] = struct{}{}
						session = append(session, m)
					}
				}
			}
		}
	} else if viewFlatIx != nil {
		for _, e := range viewFlatIx.entries[:viewFlatIx.split] {
			if !spent(e.m.Name) {
				perTest = append(perTest, e.m)
			}
		}
		for _, m := range viewFlatIx.varPerTest {
			if !spent(m.Name) {
				perTest = append(perTest, m)
			}
		}
		for _, e := range viewFlatIx.entries[viewFlatIx.split:] {
			if !spent(e.m.Name) {
				session = append(session, e.m)
			}
		}
		for _, m := range viewFlatIx.varSession {
			if !spent(m.Name) {
				session = append(session, m)
			}
		}
	}
	httpPerTest := inRecordedOrder(FilterHTTPMocks(perTest))
	httpSession := inRecordedOrder(FilterHTTPMocks(session))
	return append(append(make([]*models.Mock, 0, len(httpPerTest)+len(httpSession)), httpPerTest...), httpSession...)
}

// matchOwnedDirect scans an open test's own path bucket (or its print
// sub-bucket + rest) in recorded order using monotonic cursors over
// permanently consumed prefixes. Returns the first exact match immediately.
func (h *HTTP) matchOwnedDirect(
	ctx context.Context,
	ownIx *learnOwned,
	pathKey string,
	allRanks []int,
	meta integrations.RoundMeta,
	tracker *learn.Tracker,
	live httpParts,
	liveShape learn.Shape,
	n learnNoise,
	livePrint uint64,
	hasLivePrint bool,
) (*models.Mock, learn.Shape, httpParts, bool, bool, error) {
	var (
		ranks []int
		cur   *atomic.Int64
	)
	if len(allRanks) > 1 && hasLivePrint {
		b, err := ownIx.filePath(ctx, pathKey, allRanks, meta.Owned)
		if err != nil {
			return nil, learn.Shape{}, httpParts{}, false, false, err
		}
		same := b.byPrint[livePrint]
		rest := b.rest
		if len(same) == 0 && len(rest) == 0 {
			return nil, learn.Shape{}, httpParts{}, false, false, nil
		}
		if len(same) > 0 && len(rest) == 0 {
			ranks, cur = same, b.cursorOf(livePrint)
		} else if len(same) == 0 {
			ranks, cur = rest, &b.restCur
		} else {
			return h.matchOwnedTwoCursors(ownIx.pos, same, b.cursorOf(livePrint), rest, &b.restCur, meta, tracker, live, liveShape, n)
		}
	} else {
		ranks, cur = allRanks, ownIx.allCur[pathKey]
	}

	start := 0
	if cur != nil {
		start = int(cur.Load())
	}
	from := start
	for from < len(ranks) {
		idx, nextFrom, ok := meta.UnspentFrom(ownIx.pos, ranks, from)
		if cur != nil && from == start {
			advanceAtomicCursor(cur, nextFrom)
		}
		if !ok {
			return nil, learn.Shape{}, httpParts{}, false, false, nil
		}
		p := ownIx.pos[ranks[idx]]
		m := meta.Owned[p]
		from = idx + 1
		if isAnswered(tracker, meta, m.Name) {
			continue
		}
		fullRoundVisits.Add(1)
		rp := recordedParts(m)
		d := destOf(live, rp)
		if d != sameDest {
			continue
		}
		own, _ := ownership(m, true, nil, meta, meta, meta)
		own = own && meta.Open && !m.IsProtocolPhase()
		if !own {
			// An owned mock classified as pool cannot be skipped ahead of pool
			// handling: fall back to merged/full path.
			return nil, learn.Shape{}, httpParts{}, false, false, nil
		}
		recShape := n.shape(rp, d)
		if diff := learn.Compare(liveShape, recShape, n.known(m)); diff.Structural == "" && len(diff.Drift) == 0 && len(diff.Reordered) == 0 {
			return m, recShape, rp, len(diff.Declared) > 0, true, nil
		}
	}
	return nil, learn.Shape{}, httpParts{}, false, false, nil
}

// matchOwnedTwoCursors scans two rank-ordered slices (same-print and rest) in
// merged rank order, advancing each cursor's consumed prefix independently.
func (h *HTTP) matchOwnedTwoCursors(
	posMap []int,
	aRanks []int,
	aCur *atomic.Int64,
	bRanks []int,
	bCur *atomic.Int64,
	meta integrations.RoundMeta,
	tracker *learn.Tracker,
	live httpParts,
	liveShape learn.Shape,
	n learnNoise,
) (*models.Mock, learn.Shape, httpParts, bool, bool, error) {
	type stream struct {
		ranks []int
		cur   *atomic.Int64
		start int
		from  int
		idx   int
		has   bool
	}
	advance := func(s *stream) {
		if s.from >= len(s.ranks) {
			s.has = false
			return
		}
		idx, nextFrom, ok := meta.UnspentFrom(posMap, s.ranks, s.from)
		if s.cur != nil && s.from == s.start {
			advanceAtomicCursor(s.cur, nextFrom)
		}
		if !ok {
			s.has = false
			return
		}
		s.idx = idx
		s.from = idx + 1
		s.has = true
	}
	initStream := func(ranks []int, cur *atomic.Int64) stream {
		st := 0
		if cur != nil {
			st = int(cur.Load())
		}
		s := stream{ranks: ranks, cur: cur, start: st, from: st}
		advance(&s)
		return s
	}
	sa := initStream(aRanks, aCur)
	sb := initStream(bRanks, bCur)

	for sa.has || sb.has {
		var pick *stream
		if sa.has && (!sb.has || sa.ranks[sa.idx] < sb.ranks[sb.idx]) {
			pick = &sa
		} else {
			pick = &sb
		}
		m := meta.Owned[posMap[pick.ranks[pick.idx]]]
		advance(pick)
		if isAnswered(tracker, meta, m.Name) {
			continue
		}
		fullRoundVisits.Add(1)
		rp := recordedParts(m)
		d := destOf(live, rp)
		if d != sameDest {
			continue
		}
		own, _ := ownership(m, true, nil, meta, meta, meta)
		own = own && meta.Open && !m.IsProtocolPhase()
		if !own {
			return nil, learn.Shape{}, httpParts{}, false, false, nil
		}
		recShape := n.shape(rp, d)
		if diff := learn.Compare(liveShape, recShape, n.known(m)); diff.Structural == "" && len(diff.Drift) == 0 && len(diff.Reordered) == 0 {
			return m, recShape, rp, len(diff.Declared) > 0, true, nil
		}
	}
	return nil, learn.Shape{}, httpParts{}, false, false, nil
}

// ownedRankStream streams unspent owned recordings lazily in rank (recorded)
// order across up to 3 rank-sorted sources (same-print, rest, wild), advancing
// each cursor across permanently consumed prefixes via meta.UnspentFrom.
type ownedRankStream struct {
	meta    integrations.RoundMeta
	posMap  []int
	sources [3]ownedRankSource
	nsrc    int
	curRank int
	curMock *models.Mock
	has     bool
}

type ownedRankSource struct {
	ranks []int
	cur   *atomic.Int64
	start int
	from  int
	idx   int
	has   bool
}

func (s *ownedRankSource) step(meta integrations.RoundMeta, posMap []int) {
	if s.from >= len(s.ranks) {
		s.has = false
		return
	}
	idx, nextFrom, ok := meta.UnspentFrom(posMap, s.ranks, s.from)
	if s.cur != nil && s.from == s.start {
		advanceAtomicCursor(s.cur, nextFrom)
	}
	if !ok {
		s.has = false
		return
	}
	s.idx = idx
	s.from = idx + 1
	s.has = true
}

func newOwnedRankStream(
	ctx context.Context,
	ownIx *learnOwned,
	pathKey string,
	allRanks []int,
	wildRanks []int,
	meta integrations.RoundMeta,
	livePrint uint64,
	hasLivePrint bool,
) (*ownedRankStream, error) {
	st := &ownedRankStream{meta: meta, posMap: ownIx.pos, curRank: -1}
	addCursor := func(ranks []int, cur *atomic.Int64) {
		if len(ranks) == 0 {
			return
		}
		start := 0
		if cur != nil {
			start = int(cur.Load())
		}
		src := ownedRankSource{ranks: ranks, cur: cur, start: start, from: start}
		src.step(meta, ownIx.pos)
		if src.has {
			st.sources[st.nsrc] = src
			st.nsrc++
		}
	}
	if len(allRanks) > 0 {
		if len(allRanks) > 1 && hasLivePrint {
			b, err := ownIx.filePath(ctx, pathKey, allRanks, meta.Owned)
			if err != nil {
				return nil, err
			}
			addCursor(b.byPrint[livePrint], b.cursorOf(livePrint))
			addCursor(b.rest, &b.restCur)
		} else {
			addCursor(allRanks, ownIx.allCur[pathKey])
		}
	}
	if len(wildRanks) > 0 {
		wk := ""
		if ownIx.wildCur != nil {
			// Compute wildKey only when wildCur is populated.
			m := meta.Owned[ownIx.pos[wildRanks[0]]]
			rp := recordedParts(m)
			wk = rp.method + " " + strconv.Itoa(segmentCount(ownIx.masked[wildRanks[0]]))
		}
		addCursor(wildRanks, ownIx.wildCur[wk])
	}
	st.next()
	return st, nil
}

func (st *ownedRankStream) next() {
	for {
		best := -1
		bestRank := 0
		for i := 0; i < st.nsrc; i++ {
			if !st.sources[i].has {
				continue
			}
			r := st.sources[i].ranks[st.sources[i].idx]
			if best < 0 || r < bestRank {
				best = i
				bestRank = r
			}
		}
		if best < 0 {
			st.has = false
			st.curMock = nil
			return
		}
		src := &st.sources[best]
		r := src.ranks[src.idx]
		src.step(st.meta, st.posMap)
		if bestRank == st.curRank {
			continue // deduplicate rank present in both bucket and wild
		}
		st.curRank = bestRank
		st.curMock = st.meta.Owned[st.posMap[r]]
		st.has = true
		return
	}
}

// collectSharedUnspent returns the unspent shared index entries matching
// pathKey and wild in index order (perTest then session, each in recorded
// order). Never advances any cursor on sharedIx because sharedIx is reused
// across test views where MockSpent is not monotonic across views.
func collectSharedUnspent(
	ctx context.Context,
	ix *learnIndex,
	pathKey string,
	allIdxs []int,
	wildIdxs []int,
	spent func(string) bool,
	livePrint uint64,
	hasLivePrint bool,
) ([]learnEntry, error) {
	var idxs []int
	collect := func(ps []int) {
		for _, idx := range ps {
			if !spent(ix.entries[idx].m.Name) {
				idxs = append(idxs, idx)
			}
		}
	}
	if len(allIdxs) > 0 {
		if len(allIdxs) > 1 && hasLivePrint {
			b, err := ix.filePath(ctx, pathKey, allIdxs)
			if err != nil {
				return nil, err
			}
			collect(b.byPrint[livePrint])
			collect(b.rest)
		} else {
			collect(allIdxs)
		}
	}
	collect(wildIdxs)
	if len(idxs) == 0 {
		return nil, nil
	}
	if len(idxs) > 1 {
		slices.Sort(idxs)
		idxs = slices.Compact(idxs)
	}
	out := make([]learnEntry, len(idxs))
	for i, idx := range idxs {
		out[i] = ix.entries[idx]
	}
	return out, nil
}

// matchOpenTestMerged handles open-test lookups when shared entries, wild
// entries, or ViewAlone (!meta.Open) are present, preserving the exact scope
// merge order of scopeReads.scope while streaming owned cursors lazily so
// exact own matches resolve in O(1) amortised.
func (h *HTTP) matchOpenTestMerged(
	ctx context.Context,
	ownIx *learnOwned,
	pathKey string,
	ownRanks []int,
	ownWild []int,
	sharedIx *learnIndex,
	sharedEntries []int,
	sharedWild []int,
	meta integrations.RoundMeta,
	mockDb integrations.MockMemDb,
	tracker *learn.Tracker,
	live httpParts,
	liveShape learn.Shape,
	n learnNoise,
	opts models.OutgoingOptions,
	livePrint uint64,
	hasLivePrint bool,
) (*models.Mock, learn.Shape, httpParts, bool, bool, map[*models.Mock]bool, []*models.Mock, bool, error) {
	ownSt, err := newOwnedRankStream(ctx, ownIx, pathKey, ownRanks, ownWild, meta, livePrint, hasLivePrint)
	if err != nil {
		return nil, learn.Shape{}, httpParts{}, false, false, nil, nil, false, err
	}
	sharedList, err := collectSharedUnspent(ctx, sharedIx, pathKey, sharedEntries, sharedWild, meta.IsSpent, livePrint, hasLivePrint)
	if err != nil {
		return nil, learn.Shape{}, httpParts{}, false, false, nil, nil, false, err
	}
	if !ownSt.has && len(sharedList) == 0 {
		return nil, learn.Shape{}, httpParts{}, false, false, nil, nil, false, nil
	}
	split := 0
	for split < len(sharedList) && sharedList[split].perTest {
		split++
	}
	sharedPerTest, sharedSession := sharedList[:split], sharedList[split:]

	var (
		seenOwned []*models.Mock
		poolCands []*models.Mock
		ownSeen   map[*models.Mock]bool
	)
	checkOwn := func(m *models.Mock) (learn.Shape, httpParts, bool, bool) {
		if ownSeen == nil {
			ownSeen = map[*models.Mock]bool{}
		}
		ownSeen[m] = true
		if isAnswered(tracker, meta, m.Name) {
			return learn.Shape{}, httpParts{}, false, false
		}
		fullRoundVisits.Add(1)
		rp := recordedParts(m)
		d := destOf(live, rp)
		if d != sameDest {
			return learn.Shape{}, httpParts{}, false, false
		}
		recShape := n.shape(rp, d)
		if diff := learn.Compare(liveShape, recShape, n.known(m)); diff.Structural == "" && len(diff.Drift) == 0 && len(diff.Reordered) == 0 {
			return recShape, rp, len(diff.Declared) > 0, true
		}
		return learn.Shape{}, httpParts{}, false, false
	}

	// Phase 1a: stream httpPerTest = merge(owned, sharedPerTest) in recorded
	// order. Every own candidate in httpPerTest precedes all httpSession own
	// candidates and all pool candidates, so the first unspent own exact match
	// wins immediately without materializing remaining owned recordings.
	j := 0
	for ownSt.has || j < len(sharedPerTest) {
		var m *models.Mock
		fromOwn := ownSt.has && (j == len(sharedPerTest) || !recordedFirst(sharedPerTest[j].m, ownSt.curMock))
		if fromOwn {
			m = ownSt.curMock
			seenOwned = append(seenOwned, m)
		} else {
			m = sharedPerTest[j].m
			j++
		}
		own, _ := ownership(m, true, nil, meta, meta, meta)
		own = own && meta.Open && !m.IsProtocolPhase()
		if own {
			if recShape, rp, decl, ok := checkOwn(m); ok {
				return m, recShape, rp, true, decl, ownSeen, nil, true, nil
			}
		} else {
			poolCands = append(poolCands, m)
		}
		if fromOwn {
			ownSt.next()
		}
	}

	// Phase 1b: merge(seenOwned, sharedSession) for httpSession tier.
	i, j := 0, 0
	for i < len(seenOwned) || j < len(sharedSession) {
		var m *models.Mock
		if i < len(seenOwned) && (j == len(sharedSession) || !recordedFirst(sharedSession[j].m, seenOwned[i])) {
			m = seenOwned[i]
			i++
		} else {
			m = sharedSession[j].m
			j++
		}
		own, _ := ownership(m, false, nil, meta, meta, meta)
		own = own && meta.Open && !m.IsProtocolPhase()
		if own {
			if recShape, rp, decl, ok := checkOwn(m); ok {
				return m, recShape, rp, true, decl, ownSeen, nil, true, nil
			}
		} else {
			poolCands = append(poolCands, m)
		}
	}

	// Phase 2: check pool candidates. Only collect full poolGroup when
	// stateful cursoring applies (or ExactViewVerify is enabled).
	var (
		firstPool *models.Mock
		firstRec  learn.Shape
		firstRP   httpParts
		firstDecl bool
		poolGroup []*models.Mock
		needGroup bool
	)
	verifying := h.verifying()
	for _, m := range poolCands {
		fullRoundVisits.Add(1)
		rp := recordedParts(m)
		d := destOf(live, rp)
		if d != sameDest {
			continue
		}
		recShape := n.shape(rp, d)
		if diff := learn.Compare(liveShape, recShape, n.known(m)); diff.Structural == "" && len(diff.Drift) == 0 && len(diff.Reordered) == 0 {
			poolGroup = append(poolGroup, m)
			bg := meta.Open && !m.IsProtocolPhase() && meta.InBackground(m.Name)
			if firstPool == nil && !bg {
				firstPool, firstRec, firstRP, firstDecl = m, recShape, rp, len(diff.Declared) > 0
				needGroup = needPoolGroup(firstPool, opts, verifying)
				if !needGroup {
					return firstPool, firstRec, firstRP, false, firstDecl, ownSeen, poolGroup, true, nil
				}
			}
		}
	}
	if firstPool == nil {
		return nil, learn.Shape{}, httpParts{}, false, false, ownSeen, nil, false, nil
	}
	return firstPool, firstRec, firstRP, false, firstDecl, ownSeen, poolGroup, true, nil
}

// poolIndexStream streams unspent entries lazily in index order across up to 3
// sorted sources (same-print cursor, rest cursor, wild cursor). Safe to
// advance bucket cursors because h.learnIx.genIndexes is keyed by
// StagingGenerationByKind(models.HTTP), which moves on every view install,
// restage, gapOrder bump, and RestartExactKey, so MockSpent is strictly
// monotonic while a pool generation holds.
type poolIndexStream struct {
	entries []learnEntry
	spent   func(string) bool
	sources [3]poolIndexSource
	nsrc    int
	curIdx  int
	has     bool
}

type poolIndexSource struct {
	cur   *atomic.Int64
	idxs  []int
	start int
	from  int
	pos   int
	has   bool
}

func (s *poolIndexSource) step(entries []learnEntry, spent func(string) bool) {
	if s.from >= len(s.idxs) {
		s.has = false
		return
	}
	leading := s.cur != nil && s.from == s.start
	nextFrom := s.from
	for i := s.from; i < len(s.idxs); i++ {
		e := entries[s.idxs[i]]
		if spent(e.m.Name) {
			if leading {
				nextFrom = i + 1
			}
			continue
		}
		if leading {
			advanceAtomicCursor(s.cur, nextFrom)
		}
		s.pos = i
		s.from = i + 1
		s.has = true
		return
	}
	if leading {
		advanceAtomicCursor(s.cur, nextFrom)
	}
	s.has = false
}

func newPoolIndexStream(
	ctx context.Context,
	ix *learnIndex,
	pathKey string,
	allIdxs []int,
	wildIdxs []int,
	spent func(string) bool,
	livePrint uint64,
	hasLivePrint bool,
) (*poolIndexStream, error) {
	st := &poolIndexStream{entries: ix.entries, spent: spent, curIdx: -1}
	addCursor := func(idxs []int, cur *atomic.Int64) {
		if len(idxs) == 0 {
			return
		}
		start := 0
		if cur != nil {
			start = int(cur.Load())
		}
		src := poolIndexSource{cur: cur, idxs: idxs, start: start, from: start}
		src.step(ix.entries, spent)
		if src.has {
			st.sources[st.nsrc] = src
			st.nsrc++
		}
	}
	if len(allIdxs) > 0 {
		if len(allIdxs) > 1 && hasLivePrint {
			b, err := ix.filePath(ctx, pathKey, allIdxs)
			if err != nil {
				return nil, err
			}
			addCursor(b.byPrint[livePrint], b.cursorOf(livePrint))
			addCursor(b.rest, &b.restCur)
		} else {
			addCursor(allIdxs, ix.allCur[pathKey])
		}
	}
	if len(wildIdxs) > 0 {
		wk := ""
		if ix.wildCur != nil {
			e := ix.entries[wildIdxs[0]]
			rp := recordedParts(e.m)
			wk = rp.method + " " + strconv.Itoa(segmentCount(e.masked))
		}
		addCursor(wildIdxs, ix.wildCur[wk])
	}
	st.next()
	return st, nil
}

func (st *poolIndexStream) next() {
	for {
		best := -1
		bestIdx := 0
		for i := 0; i < st.nsrc; i++ {
			if !st.sources[i].has {
				continue
			}
			idx := st.sources[i].idxs[st.sources[i].pos]
			if best < 0 || idx < bestIdx {
				best = i
				bestIdx = idx
			}
		}
		if best < 0 {
			st.has = false
			return
		}
		st.sources[best].step(st.entries, st.spent)
		if bestIdx == st.curIdx {
			continue
		}
		st.curIdx = bestIdx
		st.has = true
		return
	}
}

// matchPoolView resolves an exact pool match when no test is open
// (meta.Test == "", !meta.Ahead), streaming unspent entries lazily in O(1)
// amortised and stopping at the first exact match unless stateful cursoring
// applies (or ExactViewVerify is enabled).
func (h *HTTP) matchPoolView(
	ctx context.Context,
	poolIx *learnIndex,
	pathKey string,
	poolEntries []int,
	poolWild []int,
	meta integrations.RoundMeta,
	mockDb integrations.MockMemDb,
	live httpParts,
	liveShape learn.Shape,
	n learnNoise,
	opts models.OutgoingOptions,
	livePrint uint64,
	hasLivePrint bool,
) (*models.Mock, learn.Shape, httpParts, bool, []*models.Mock, bool, error) {
	st, err := newPoolIndexStream(ctx, poolIx, pathKey, poolEntries, poolWild, meta.IsSpent, livePrint, hasLivePrint)
	if err != nil || !st.has {
		return nil, learn.Shape{}, httpParts{}, false, nil, false, err
	}
	// When !meta.Open (pool view), ownership() has own = own && meta.Open = false
	// for every candidate, and Background = !own && meta.Open = false.
	var (
		firstPool *models.Mock
		firstRec  learn.Shape
		firstRP   httpParts
		firstDecl bool
		poolGroup []*models.Mock
		needGroup bool
	)
	verifying := h.verifying()
	for st.has {
		m := poolIx.entries[st.curIdx].m
		st.next()
		fullRoundVisits.Add(1)
		rp := recordedParts(m)
		d := destOf(live, rp)
		if d != sameDest {
			continue
		}
		recShape := n.shape(rp, d)
		if diff := learn.Compare(liveShape, recShape, n.known(m)); diff.Structural == "" && len(diff.Drift) == 0 && len(diff.Reordered) == 0 {
			poolGroup = append(poolGroup, m)
			if firstPool == nil {
				firstPool, firstRec, firstRP, firstDecl = m, recShape, rp, len(diff.Declared) > 0
				needGroup = needPoolGroup(firstPool, opts, verifying)
				if !needGroup {
					return firstPool, firstRec, firstRP, firstDecl, poolGroup, true, nil
				}
			}
		}
	}
	if firstPool == nil {
		return nil, learn.Shape{}, httpParts{}, false, nil, false, nil
	}
	return firstPool, firstRec, firstRP, firstDecl, poolGroup, true, nil
}
