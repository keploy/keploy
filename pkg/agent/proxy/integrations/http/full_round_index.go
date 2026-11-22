package http

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// Record's full-round exact-match index (learnMatchIn).
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
// fullRoundIndexes caches per-view/per-generation indexes (completely separate
// from replay's strictIx and learnIx) keyed by method + masked path, and lazily
// files multi-entry path buckets by fullRoundPrint (media type, masked query,
// and canonical JSON/raw body hash) with monotonically advancing cursors over
// permanently consumed/spent prefixes. Exact-match calls resolve in O(1)
// amortised work without taking a full RoundSnapshot; any drift, miss,
// declared-noise rewrite, store read-back, background-only match, or boundary
// diagnostic falls back to the full readLearnPass + learn.Pair path.

var (
	fullRoundIndexBuilds      atomic.Int64
	fullRoundBucketFilings    atomic.Int64
	fullRoundFastHits         atomic.Int64
	fullRoundVisits           atomic.Int64
	fullRoundRetakePoolBuilds atomic.Int64
)

// fullRoundIndexes holds the cached full-round exact-match indexes for one
// HTTP integration instance.
type fullRoundIndexes struct {
	owned  fullRoundOwnedCache
	shared fullRoundFlatCache
	pool   fullRoundFlatCache
}

// ---- owned index (open test's v.owned recordings) ----

type fullRoundOwnedKey struct {
	store integrations.MockMemDb
	view  uint64
	url   string
}

type fullRoundOwnedCache struct {
	mu   sync.Mutex
	key  fullRoundOwnedKey
	last *fullRoundOwnedIndex
}

func (c *fullRoundOwnedCache) of(base integrations.MockMemDb, sr integrations.SharedReads, view uint64, owned []*models.Mock, n learnNoise, urlKey string) *fullRoundOwnedIndex {
	if base == nil || reflect.ValueOf(base).Kind() != reflect.Pointer {
		return nil
	}
	key := fullRoundOwnedKey{store: base, view: view, url: urlKey}
	c.mu.Lock()
	if c.last != nil && c.key == key {
		ix := c.last
		c.mu.Unlock()
		return ix
	}
	c.last, c.key = nil, fullRoundOwnedKey{}
	c.mu.Unlock()
	integrations.WhenRestaged(base, c, func() { c.release(base) })
	ix := buildFullRoundOwnedIndex(owned, n)
	c.mu.Lock()
	c.last, c.key = ix, key
	c.mu.Unlock()
	if again, _, ok := sr.OwnedRecordings(); !ok || again != view {
		c.release(base)
		return nil
	}
	return ix
}

func (c *fullRoundOwnedCache) release(store integrations.MockMemDb) {
	c.mu.Lock()
	if c.last != nil && c.key.store == store {
		c.last, c.key = nil, fullRoundOwnedKey{}
	}
	c.mu.Unlock()
}

type fullRoundOwnedIndex struct {
	// pos[r] is the position in v.owned of rank r (in inRecordedOrder order).
	pos    []int
	byPath map[string]*fullRoundOwnedBucket
	wild   map[string]*fullRoundOwnedCursor // method+" "+segCount -> cursor
}

type fullRoundOwnedCursor struct {
	ranks    []int
	ownedPos []int
	next     atomic.Int64
}

func (c *fullRoundOwnedCursor) advance(next int) {
	for {
		cur := int(c.next.Load())
		if next <= cur {
			return
		}
		if c.next.CompareAndSwap(int64(cur), int64(next)) {
			return
		}
	}
}

type fullRoundOwnedPrintMap struct {
	byPrint map[uint64]*fullRoundOwnedCursor
	rest    *fullRoundOwnedCursor
}

type fullRoundOwnedBucket struct {
	all   fullRoundOwnedCursor
	turn  chan struct{}
	filed atomic.Pointer[fullRoundOwnedPrintMap]
}

func buildFullRoundOwnedIndex(owned []*models.Mock, n learnNoise) *fullRoundOwnedIndex {
	fullRoundIndexBuilds.Add(1)
	ix := &fullRoundOwnedIndex{
		byPath: make(map[string]*fullRoundOwnedBucket),
		wild:   make(map[string]*fullRoundOwnedCursor),
	}
	for p, m := range owned {
		if m != nil && m.Kind == models.HTTP && m.Spec.HTTPReq != nil && !hasVariables(m) {
			ix.pos = append(ix.pos, p)
		}
	}
	sort.SliceStable(ix.pos, func(i, j int) bool {
		return recordedFirst(owned[ix.pos[i]], owned[ix.pos[j]])
	})
	for r, p := range ix.pos {
		m := owned[p]
		rp := recordedParts(m)
		masked := n.maskedPath(rp.path)
		pk := rp.method + " " + masked
		b := ix.byPath[pk]
		if b == nil {
			b = &fullRoundOwnedBucket{}
			ix.byPath[pk] = b
		} else if b.turn == nil {
			// Allocate turn channel only for multi-entry buckets that can be filed.
			b.turn = make(chan struct{}, 1)
		}
		b.all.ranks = append(b.all.ranks, r)
		b.all.ownedPos = append(b.all.ownedPos, p)
		if learnsWide(m) {
			wk := rp.method + " " + strconv.Itoa(segmentCount(masked))
			wc := ix.wild[wk]
			if wc == nil {
				wc = &fullRoundOwnedCursor{}
				ix.wild[wk] = wc
			}
			wc.ranks = append(wc.ranks, r)
			wc.ownedPos = append(wc.ownedPos, p)
		}
	}
	return ix
}

func (b *fullRoundOwnedBucket) file(ctx context.Context, owned []*models.Mock, n learnNoise) (*fullRoundOwnedPrintMap, error) {
	if pm := b.filed.Load(); pm != nil {
		return pm, nil
	}
	select {
	case b.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-b.turn }()
	if pm := b.filed.Load(); pm != nil {
		return pm, nil
	}
	pm := &fullRoundOwnedPrintMap{byPrint: make(map[uint64]*fullRoundOwnedCursor)}
	var restRanks, restPos []int
	for i, r := range b.all.ranks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := b.all.ownedPos[i]
		m := owned[p]
		if sum, ok := fullRoundPlainPrint(m, recordedParts(m), n); ok {
			cur := pm.byPrint[sum]
			if cur == nil {
				cur = &fullRoundOwnedCursor{}
				pm.byPrint[sum] = cur
			}
			cur.ranks = append(cur.ranks, r)
			cur.ownedPos = append(cur.ownedPos, p)
		} else {
			restRanks = append(restRanks, r)
			restPos = append(restPos, p)
		}
	}
	if len(restRanks) > 0 {
		pm.rest = &fullRoundOwnedCursor{ranks: restRanks, ownedPos: restPos}
	}
	fullRoundBucketFilings.Add(1)
	b.filed.Store(pm)
	return pm, nil
}

// ---- flat index (shared part of open test view, or pool view) ----

type fullRoundFlatKey struct {
	store   integrations.MockMemDb
	shared  integrations.SharedGen
	gen     uint64
	viewGen uint64
	url     string
}

type fullRoundFlatCache struct {
	mu   sync.Mutex
	key  fullRoundFlatKey
	seen fullRoundFlatKey
	last *fullRoundFlatIndex
}

func (c *fullRoundFlatCache) ofShared(base integrations.MockMemDb, sr integrations.SharedReads, sg integrations.SharedGen, n learnNoise, urlKey string) *fullRoundFlatIndex {
	if base == nil || reflect.ValueOf(base).Kind() != reflect.Pointer {
		return nil
	}
	key := fullRoundFlatKey{store: base, shared: sg, url: urlKey}
	c.mu.Lock()
	if c.last != nil && c.key == key {
		ix := c.last
		c.mu.Unlock()
		return ix
	}
	c.last, c.key = nil, fullRoundFlatKey{}
	c.mu.Unlock()
	integrations.WhenRestaged(base, c, func() { c.release(base) })
	perTest, session, ok := sr.SharedMocks()
	if !ok {
		return nil
	}
	if again, ok := sr.SharedGeneration(models.HTTP); !ok || again != sg {
		return nil
	}
	ix := buildFullRoundFlatIndex(perTest, session, n)
	c.mu.Lock()
	c.last, c.key = ix, key
	c.mu.Unlock()
	if again, ok := sr.SharedGeneration(models.HTTP); !ok || again != sg {
		c.release(base)
		return nil
	}
	return ix
}

func (c *fullRoundFlatCache) ofPool(base, db integrations.MockMemDb, gen, viewGen uint64, n learnNoise, urlKey string) *fullRoundFlatIndex {
	if base == nil || reflect.ValueOf(base).Kind() != reflect.Pointer || !integrations.KeptGeneration(gen) {
		return nil
	}
	key := fullRoundFlatKey{store: base, gen: gen, viewGen: viewGen, url: urlKey}
	c.mu.Lock()
	if c.last != nil && c.key == key {
		ix := c.last
		c.mu.Unlock()
		return ix
	}
	second := c.seen == key
	c.seen = key
	c.last, c.key = nil, fullRoundFlatKey{}
	c.mu.Unlock()
	integrations.WhenRestaged(base, c, func() { c.release(base) })
	if !second {
		return nil
	}
	perTest, err := db.GetPerTestMocksInWindow()
	if err != nil {
		return nil
	}
	//nolint:staticcheck // SA1019: matches readLearnPass/RoundSnapshot session pool union.
	session, err := db.GetSessionMocks()
	if err != nil {
		return nil
	}
	if again, ok := integrations.StagingGenerationOf(db, models.HTTP); !ok || again != gen {
		return nil
	}
	ix := buildFullRoundFlatIndex(perTest, session, n)
	c.mu.Lock()
	c.last, c.key = ix, key
	c.mu.Unlock()
	if again, ok := integrations.StagingGenerationOf(db, models.HTTP); !ok || again != gen {
		c.release(base)
		return nil
	}
	return ix
}

func (c *fullRoundFlatCache) release(store integrations.MockMemDb) {
	c.mu.Lock()
	if c.last != nil && c.key.store == store {
		c.last, c.key = nil, fullRoundFlatKey{}
	}
	if c.seen.store == store {
		c.seen = fullRoundFlatKey{}
	}
	c.mu.Unlock()
}

type fullRoundFlatEntry struct {
	m       *models.Mock
	perTest bool
}

type fullRoundFlatCursor struct {
	idx  []int
	next atomic.Int64
}

func (c *fullRoundFlatCursor) advance(next int) {
	for {
		cur := int(c.next.Load())
		if next <= cur {
			return
		}
		if c.next.CompareAndSwap(int64(cur), int64(next)) {
			return
		}
	}
}

type fullRoundFlatPrintMap struct {
	byPrint map[uint64]*fullRoundFlatCursor
	rest    *fullRoundFlatCursor
}

type fullRoundFlatBucket struct {
	all   fullRoundFlatCursor
	turn  chan struct{}
	filed atomic.Pointer[fullRoundFlatPrintMap]
}

type fullRoundFlatIndex struct {
	entries    []fullRoundFlatEntry
	split      int
	varPerTest []*models.Mock
	varSession []*models.Mock
	byPath     map[string]*fullRoundFlatBucket
	wild       map[string]*fullRoundFlatCursor
}

func buildFullRoundFlatIndex(perTest, session []*models.Mock, n learnNoise) *fullRoundFlatIndex {
	fullRoundIndexBuilds.Add(1)
	ix := &fullRoundFlatIndex{
		byPath: make(map[string]*fullRoundFlatBucket),
		wild:   make(map[string]*fullRoundFlatCursor),
	}
	add := func(mocks []*models.Mock, isPerTest bool) {
		for _, m := range inRecordedOrder(FilterHTTPMocks(mocks)) {
			if m == nil || m.Spec.HTTPReq == nil {
				continue
			}
			if hasVariables(m) {
				if isPerTest {
					ix.varPerTest = append(ix.varPerTest, m)
				} else {
					ix.varSession = append(ix.varSession, m)
				}
				continue
			}
			rp := recordedParts(m)
			masked := n.maskedPath(rp.path)
			idx := len(ix.entries)
			ix.entries = append(ix.entries, fullRoundFlatEntry{m: m, perTest: isPerTest})
			pk := rp.method + " " + masked
			b := ix.byPath[pk]
			if b == nil {
				b = &fullRoundFlatBucket{}
				ix.byPath[pk] = b
			} else if b.turn == nil {
				b.turn = make(chan struct{}, 1)
			}
			b.all.idx = append(b.all.idx, idx)
			if learnsWide(m) {
				wk := rp.method + " " + strconv.Itoa(segmentCount(masked))
				wc := ix.wild[wk]
				if wc == nil {
					wc = &fullRoundFlatCursor{}
					ix.wild[wk] = wc
				}
				wc.idx = append(wc.idx, idx)
			}
		}
	}
	add(perTest, true)
	ix.split = len(ix.entries)
	add(session, false)
	return ix
}

func (b *fullRoundFlatBucket) file(ctx context.Context, entries []fullRoundFlatEntry, n learnNoise) (*fullRoundFlatPrintMap, error) {
	if pm := b.filed.Load(); pm != nil {
		return pm, nil
	}
	select {
	case b.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-b.turn }()
	if pm := b.filed.Load(); pm != nil {
		return pm, nil
	}
	pm := &fullRoundFlatPrintMap{byPrint: make(map[uint64]*fullRoundFlatCursor)}
	var rest []int
	for _, idx := range b.all.idx {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m := entries[idx].m
		if sum, ok := fullRoundPlainPrint(m, recordedParts(m), n); ok {
			cur := pm.byPrint[sum]
			if cur == nil {
				cur = &fullRoundFlatCursor{}
				pm.byPrint[sum] = cur
			}
			cur.idx = append(cur.idx, idx)
		} else {
			rest = append(rest, idx)
		}
	}
	if len(rest) > 0 {
		pm.rest = &fullRoundFlatCursor{idx: rest}
	}
	fullRoundBucketFilings.Add(1)
	b.filed.Store(pm)
	return pm, nil
}

// ---- full-round request print (mediaType + masked query + canonical body) ----

// fullRoundPlainPrint returns the print of recorded request m (parts rp) under
// url noise n.url when an exact match in learn.Compare requires the live
// request to share this print (assuming n.body.Empty()). Returns ok=false for
// recordings with learned noise, obfuscation noise, form/multipart bodies, or
// JSON keys containing '.' or '[' (leaf path collision).
func fullRoundPlainPrint(m *models.Mock, rp httpParts, n learnNoise) (uint64, bool) {
	if len(m.Spec.ReqBodyNoise) > 0 || len(m.Noise) > 0 {
		return 0, false
	}
	return fullRoundPartsPrint(rp, n)
}

// fullRoundLivePrint returns the print of the live request when keploy.yml
// declares no request-body noise.
func fullRoundLivePrint(live httpParts, n learnNoise) (uint64, bool) {
	if n.hasBodyNoise {
		return 0, false
	}
	return fullRoundPartsPrint(live, n)
}

func fullRoundPartsPrint(p httpParts, n learnNoise) (uint64, bool) {
	switch p.mediaType {
	case "application/x-www-form-urlencoded", "multipart/form-data":
		return 0, false
	}
	h := fnv.New64a()
	var buf [binary.MaxVarintLen64]byte
	tag := func(t byte, sz int) {
		buf[0] = t
		_, _ = h.Write(buf[:1+binary.PutUvarint(buf[1:], uint64(sz))])
	}
	// 1. Media type (fixed in learnNoise.shape).
	tag('M', len(p.mediaType))
	_, _ = io.WriteString(h, p.mediaType)

	// 2. Masked query parameters (skipping keploy*, matching learnNoise.shape).
	if len(p.query) > 0 {
		keys := make([]string, 0, len(p.query))
		for k := range p.query {
			if strings.HasPrefix(strings.ToLower(k), "keploy") {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		tag('Q', len(keys))
		for _, k := range keys {
			tag('k', len(k))
			_, _ = io.WriteString(h, k)
			vs := p.query[k]
			if len(vs) == 1 {
				mv := maskURLNoise(vs[0], n.url)
				tag('v', len(mv))
				_, _ = io.WriteString(h, mv)
				continue
			}
			masked := make([]string, len(vs))
			for i, v := range vs {
				masked[i] = maskURLNoise(v, n.url)
			}
			sort.Strings(masked)
			joined := strings.Join(masked, "\x00")
			tag('m', len(joined))
			_, _ = io.WriteString(h, joined)
		}
	} else {
		tag('Q', 0)
	}

	// 3. Body (matching learnNoise.shape: empty / JSON leaves / raw text).
	if len(p.body) == 0 {
		tag('E', 0)
		return h.Sum64(), true
	}
	v, isJSON := learn.DecodeJSON(p.body)
	if !isJSON {
		tag('T', len(p.body))
		_, _ = h.Write(p.body)
		return h.Sum64(), true
	}
	tag('J', 0)
	if !writeFullRoundJSON(h, v) {
		return 0, false
	}
	return h.Sum64(), true
}

func writeFullRoundJSON(h hash.Hash64, v interface{}) bool {
	var buf [binary.MaxVarintLen64]byte
	tag := func(t byte, n int) {
		buf[0] = t
		_, _ = h.Write(buf[:1+binary.PutUvarint(buf[1:], uint64(n))])
	}
	switch t := v.(type) {
	case map[string]interface{}:
		if len(t) == 0 {
			tag('O', 0)
			return true
		}
		tag('o', len(t))
		keys := make([]string, 0, len(t))
		for k := range t {
			if strings.ContainsAny(k, ".[") {
				return false
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			tag('k', len(k))
			_, _ = io.WriteString(h, k)
			if !writeFullRoundJSON(h, t[k]) {
				return false
			}
		}
		return true
	case []interface{}:
		if len(t) == 0 {
			tag('A', 0)
			return true
		}
		tag('a', len(t))
		for _, e := range t {
			if !writeFullRoundJSON(h, e) {
				return false
			}
		}
		return true
	case string:
		tag('s', len(t))
		_, _ = io.WriteString(h, t)
		return true
	case json.Number:
		s := t.String()
		tag('n', len(s))
		_, _ = io.WriteString(h, s)
		return true
	case float64:
		s := strconv.FormatFloat(t, 'g', -1, 64)
		tag('n', len(s))
		_, _ = io.WriteString(h, s)
		return true
	case bool:
		if t {
			tag('t', 0)
		} else {
			tag('f', 0)
		}
		return true
	case nil:
		tag('z', 0)
		return true
	default:
		return false
	}
}

// ---- exact-match fast path invoked by learnMatchIn ----

type fullRoundStep uint8

const (
	fullRoundFallback fullRoundStep = iota
	fullRoundServed
	fullRoundMiss
	fullRoundRetake
)

type fullRoundOutcome struct {
	step fullRoundStep
	out  *models.Mock
	miss *learnMiss
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
	t, ok := putTarget(read)
	if !ok {
		return false
	}
	for _, p := range tracker.HTTPPuts() {
		if pt, pok := putTarget(p); pok && pt == t {
			return true
		}
	}
	return false
}

// tryExactLearnMatch attempts to resolve a single pass of learnMatchIn via
// O(1) RoundMeta + fullRoundIndexes. It serves ONLY when it proves an
// unspent, non-background exact match with no declared-noise rewrite and no
// pending read-back write; every other case returns fullRoundFallback so
// learnMatchIn runs the full readLearnPass + learn.Pair path.
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
	call *integrations.CallRound,
	marked *bool,
	made *learnRound,
	madePool *[]*models.Mock,
	madePoolFn *func() []*models.Mock,
	alone *string,
	retake bool,
	unclaimed map[*models.Mock]learnPass,
) (fullRoundOutcome, error) {
	rev := storeRevision(mockDb)
	meta, ok := integrations.ReadRoundMeta(mockDb, tracker)
	if !ok {
		return fullRoundOutcome{step: fullRoundFallback}, nil
	}
	nowPass := learnPass{rev: rev, round: meta.At}
	nowRound := learnRound{
		read:    true,
		open:    meta.LearnOpen,
		test:    meta.LearnNumber,
		snap:    true,
		agrees:  meta.Phase == integrations.RoundInTest || meta.Phase == integrations.RoundBetween,
		epoch:   meta.At.Epoch,
		viewGen: meta.ViewGen(),
		view:    meta.Test,
		noOne:   meta.NoOneRound(),
		ahead:   meta.TrackerAhead(),
		alone:   meta.ViewAlone(),
	}
	if !made.read && mark.MovedSince(nowPass.round) {
		res := learn.Result{Outcome: learn.Miss, Index: -1, Why: markMovedWhy(mark, nowRound.snap, nowPass.round)}
		return fullRoundOutcome{step: fullRoundMiss, miss: &learnMiss{res: res, moved: true}}, nil
	}
	moved, caughtUp := made.movedFrom(nowRound, retake)
	if moved != "" || nowRound.noOne != "" {
		// Rare moved-round or boundary miss: fall back to full readLearnPass
		// so the miss carries the exact HTTP pool diagnostic.
		return fullRoundOutcome{step: fullRoundFallback}, nil
	}
	nextMade := *made
	if !nextMade.read || caughtUp {
		nextMade = nowRound
	}
	if (nextMade.waits() && nowRound.ahead) || meta.LearnAwaiting {
		// Rare boundary wait or request-awaiting pause: fall back to
		// readLearnPass so retake reset and madePool stay identical.
		return fullRoundOutcome{step: fullRoundFallback}, nil
	}
	commitRound := func(poolFn func() []*models.Mock) {
		if call != nil && (!*marked || caughtUp) {
			*call, *marked = meta.CallRound(), true
		}
		if !made.read {
			*alone = nowRound.alone
		}
		if !made.read || caughtUp {
			*made = nowRound
			*madePool = nil
			*madePoolFn = poolFn
		}
	}

	stuck := func(m *models.Mock) bool {
		at, exists := unclaimed[m]
		return exists && at == nowPass
	}

	urlKey := urlNoiseKey(n.url)
	liveMasked := n.maskedPath(live.path)
	pathKey := live.method + " " + liveMasked
	wildKey := live.method + " " + strconv.Itoa(segmentCount(liveMasked))
	livePrint, hasLivePrint := fullRoundLivePrint(live, n)

	var (
		firstMatch  *models.Mock
		firstRec    learn.Shape
		firstRP     httpParts
		firstOwn    bool
		hasDeclared bool
		ownSeen     map[*models.Mock]bool
		poolGroup   []*models.Mock
		viewFlatIx  *fullRoundFlatIndex
	)

	// Case A: open test view (meta.Test != "").
	if meta.Test != "" {
		sr, ok := mockDb.(integrations.SharedReads)
		if !ok {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		ownIx := h.fullRoundIx.owned.of(mockDb, sr, meta.OwnedGen, meta.Owned, n, urlKey)
		sharedIx := h.fullRoundIx.shared.ofShared(mockDb, sr, meta.SharedGen, n, urlKey)
		if ownIx == nil || sharedIx == nil {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		viewFlatIx = sharedIx
		ownBucket := ownIx.byPath[pathKey]
		ownWild := ownIx.wild[wildKey]
		sharedBucket := sharedIx.byPath[pathKey]
		sharedWild := sharedIx.wild[wildKey]

		// Fast path A1: open test agreeing with tracker, no matching shared
		// entries, no matching wild entries in owned -> scan owned bucket
		// directly in O(1) amortised without allocating candidate lists.
		if meta.Open && sharedBucket == nil && sharedWild == nil && ownWild == nil {
			if ownBucket == nil {
				return fullRoundOutcome{step: fullRoundFallback}, nil
			}
			m, recShape, rp, decl, matched, err := h.matchOwnedDirect(ctx, ownBucket, meta, tracker, live, liveShape, n, livePrint, hasLivePrint)
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
				ctx, ownBucket, ownWild, sharedIx, sharedBucket, sharedWild,
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
		// Key poolIx by meta.At.Gen (not ViewGen) so RestartExactKey's
		// restoreLocked bump immediately invalidates pool cursors.
		poolIx := h.fullRoundIx.pool.ofPool(mockDb, mockDb, meta.StagingGen, meta.At.Gen, n, urlKey)
		if poolIx == nil {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		viewFlatIx = poolIx
		poolBucket := poolIx.byPath[pathKey]
		poolWild := poolIx.wild[wildKey]
		if poolBucket == nil && poolWild == nil {
			return fullRoundOutcome{step: fullRoundFallback}, nil
		}
		m, recShape, rp, decl, pg, matched, err := h.matchPoolView(
			ctx, poolIx, poolBucket, poolWild, meta, mockDb, live, liveShape, n, opts, livePrint, hasLivePrint,
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
			}
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

	if firstOwn && !tracker.Answer(meta.LearnNumber, picked) {
		commitRound(func() []*models.Mock {
			return h.buildRetakePool(mockDb, tracker, meta, viewFlatIx, live, liveShape, n, opts.LearnPool)
		})
		return fullRoundOutcome{step: fullRoundRetake}, nil
	}

	claimDb := integrations.WithinRound(mockDb, meta.At)
	ownerFn := func(m *models.Mock) bool { return m == picked && firstOwn }
	out, claimed, err := h.claim(context.WithValue(ctx, ownedKey{}, ownerFn), picked, claimDb, nil, nil)
	if err != nil || !claimed {
		if firstOwn {
			tracker.Unanswer(meta.LearnNumber, picked.Name)
		}
		if err != nil {
			return fullRoundOutcome{}, err
		}
		unclaimed[picked] = nowPass
		commitRound(func() []*models.Mock {
			return h.buildRetakePool(mockDb, tracker, meta, viewFlatIx, live, liveShape, n, opts.LearnPool)
		})
		return fullRoundOutcome{step: fullRoundRetake}, nil
	}
	commitRound(nil)
	commit()
	if e, ok := n.servedExchange(tracker.Drifts(), picked, nil, rec, rp, liveShape, input, live, out, firstOwn, answerView{}); ok {
		tracker.Served(meta.LearnNumber, learn.Named(e, nil, nil))
	}
	fullRoundFastHits.Add(1)
	return fullRoundOutcome{step: fullRoundServed, out: out}, nil
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
	viewFlatIx *fullRoundFlatIndex,
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
		allPos := make([]int, len(meta.Owned))
		for i := range allPos {
			allPos[i] = i
		}
		for from := 0; from < len(allPos); {
			idx, _, ok := meta.UnspentFrom(allPos, from)
			if !ok {
				break
			}
			ownedUnspent = append(ownedUnspent, meta.Owned[allPos[idx]])
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

// matchOwnedDirect scans an open test's own path bucket directly using its
// print sub-bucket and monotonically advancing cursor. Returns matched=false
// to trigger fallback if any non-own entry is encountered or no exact match is
// found.
func (h *HTTP) matchOwnedDirect(
	ctx context.Context,
	bucket *fullRoundOwnedBucket,
	meta integrations.RoundMeta,
	tracker *learn.Tracker,
	live httpParts,
	liveShape learn.Shape,
	n learnNoise,
	livePrint uint64,
	hasLivePrint bool,
) (*models.Mock, learn.Shape, httpParts, bool, bool, error) {
	var cur *fullRoundOwnedCursor
	if len(bucket.all.ranks) > 1 && hasLivePrint {
		pm, err := bucket.file(ctx, meta.Owned, n)
		if err != nil {
			return nil, learn.Shape{}, httpParts{}, false, false, err
		}
		cSame := pm.byPrint[livePrint]
		cRest := pm.rest
		if cSame == nil && cRest == nil {
			return nil, learn.Shape{}, httpParts{}, false, false, nil
		}
		if cSame != nil && cRest == nil {
			cur = cSame
		} else if cSame == nil {
			cur = cRest
		} else {
			// Both cSame and cRest are non-empty: iterate them in merged rank order.
			return h.matchOwnedTwoCursors(cSame, cRest, meta, tracker, live, liveShape, n)
		}
	} else {
		cur = &bucket.all
	}

	start := int(cur.next.Load())
	from := start
	for from < len(cur.ownedPos) {
		idx, nextFrom, ok := meta.UnspentFrom(cur.ownedPos, from)
		if from == start {
			cur.advance(nextFrom)
		}
		if !ok {
			return nil, learn.Shape{}, httpParts{}, false, false, nil
		}
		p := cur.ownedPos[idx]
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

// matchOwnedTwoCursors scans two rank-ordered cursors (same-print and rest) in
// merged rank order, advancing each cursor's consumed prefix independently.
func (h *HTTP) matchOwnedTwoCursors(
	a, b *fullRoundOwnedCursor,
	meta integrations.RoundMeta,
	tracker *learn.Tracker,
	live httpParts,
	liveShape learn.Shape,
	n learnNoise,
) (*models.Mock, learn.Shape, httpParts, bool, bool, error) {
	type stream struct {
		cur   *fullRoundOwnedCursor
		start int
		from  int
		idx   int
		has   bool
	}
	advance := func(s *stream) {
		if s.from >= len(s.cur.ownedPos) {
			s.has = false
			return
		}
		idx, nextFrom, ok := meta.UnspentFrom(s.cur.ownedPos, s.from)
		if s.from == s.start {
			s.cur.advance(nextFrom)
		}
		if !ok {
			s.has = false
			return
		}
		s.idx = idx
		s.from = idx + 1
		s.has = true
	}
	sa := stream{cur: a, start: int(a.next.Load())}
	sa.from = sa.start
	sb := stream{cur: b, start: int(b.next.Load())}
	sb.from = sb.start
	advance(&sa)
	advance(&sb)

	for sa.has || sb.has {
		var pick *stream
		if sa.has && (!sb.has || sa.cur.ranks[sa.idx] < sb.cur.ranks[sb.idx]) {
			pick = &sa
		} else {
			pick = &sb
		}
		m := meta.Owned[pick.cur.ownedPos[pick.idx]]
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
// order across up to 3 rank-sorted sources (same-print cursor, rest cursor,
// wild ranks), advancing each bucket cursor across permanently consumed
// prefixes via meta.UnspentFrom.
type ownedRankStream struct {
	meta    integrations.RoundMeta
	sources [3]ownedRankSource
	nsrc    int
	curRank int
	curMock *models.Mock
	has     bool
}

type ownedRankSource struct {
	cur   *fullRoundOwnedCursor
	ranks []int
	pos   []int
	start int
	from  int
	idx   int
	has   bool
}

func (s *ownedRankSource) step(meta integrations.RoundMeta) {
	if s.from >= len(s.pos) {
		s.has = false
		return
	}
	idx, nextFrom, ok := meta.UnspentFrom(s.pos, s.from)
	if s.cur != nil && s.from == s.start {
		s.cur.advance(nextFrom)
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
	bucket *fullRoundOwnedBucket,
	wild *fullRoundOwnedCursor,
	meta integrations.RoundMeta,
	n learnNoise,
	livePrint uint64,
	hasLivePrint bool,
) (*ownedRankStream, error) {
	st := &ownedRankStream{meta: meta, curRank: -1}
	addCursor := func(c *fullRoundOwnedCursor) {
		if c == nil || len(c.ownedPos) == 0 {
			return
		}
		start := int(c.next.Load())
		src := ownedRankSource{cur: c, ranks: c.ranks, pos: c.ownedPos, start: start, from: start}
		src.step(meta)
		if src.has {
			st.sources[st.nsrc] = src
			st.nsrc++
		}
	}
	if bucket != nil {
		if len(bucket.all.ranks) > 1 && hasLivePrint {
			pm, err := bucket.file(ctx, meta.Owned, n)
			if err != nil {
				return nil, err
			}
			addCursor(pm.byPrint[livePrint])
			addCursor(pm.rest)
		} else {
			addCursor(&bucket.all)
		}
	}
	addCursor(wild)
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
		p := src.pos[src.idx]
		src.step(st.meta)
		if bestRank == st.curRank {
			continue // deduplicate rank present in both bucket and wild
		}
		st.curRank = bestRank
		st.curMock = st.meta.Owned[p]
		st.has = true
		return
	}
}

// collectSharedUnspent returns the unspent shared index entries matching bucket
// and wild in index order (perTest then session, each in recorded order).
// Never advances any cursor on sharedIx because sharedIx is reused across test
// views where MockSpent (v.spent, v.spentOutsideIn, takenByOther) is not
// monotonic across views.
func collectSharedUnspent(
	ctx context.Context,
	ix *fullRoundFlatIndex,
	bucket *fullRoundFlatBucket,
	wild *fullRoundFlatCursor,
	spent func(string) bool,
	n learnNoise,
	livePrint uint64,
	hasLivePrint bool,
) ([]fullRoundFlatEntry, error) {
	var idxs []int
	collect := func(c *fullRoundFlatCursor) {
		if c == nil {
			return
		}
		for _, idx := range c.idx {
			if !spent(ix.entries[idx].m.Name) {
				idxs = append(idxs, idx)
			}
		}
	}
	if bucket != nil {
		if len(bucket.all.idx) > 1 && hasLivePrint {
			pm, err := bucket.file(ctx, ix.entries, n)
			if err != nil {
				return nil, err
			}
			collect(pm.byPrint[livePrint])
			collect(pm.rest)
		} else {
			collect(&bucket.all)
		}
	}
	collect(wild)
	if len(idxs) == 0 {
		return nil, nil
	}
	if len(idxs) > 1 {
		slices.Sort(idxs)
		idxs = slices.Compact(idxs)
	}
	out := make([]fullRoundFlatEntry, len(idxs))
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
	ownBucket *fullRoundOwnedBucket,
	ownWild *fullRoundOwnedCursor,
	sharedIx *fullRoundFlatIndex,
	sharedBucket *fullRoundFlatBucket,
	sharedWild *fullRoundFlatCursor,
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
	ownSt, err := newOwnedRankStream(ctx, ownBucket, ownWild, meta, n, livePrint, hasLivePrint)
	if err != nil {
		return nil, learn.Shape{}, httpParts{}, false, false, nil, nil, false, err
	}
	sharedList, err := collectSharedUnspent(ctx, sharedIx, sharedBucket, sharedWild, mockSpentOf(mockDb), n, livePrint, hasLivePrint)
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
// advance bucket cursors because poolIx is keyed by (store, StagingGen,
// meta.At.Gen, url) and in a pool view (meta.Test == "", !meta.Ahead) MockSpent
// is strictly monotonic within that view and staging.
type poolIndexStream struct {
	entries []fullRoundFlatEntry
	spent   func(string) bool
	sources [3]poolIndexSource
	nsrc    int
	curIdx  int
	has     bool
}

type poolIndexSource struct {
	cur   *fullRoundFlatCursor
	idxs  []int
	start int
	from  int
	pos   int
	has   bool
}

func (s *poolIndexSource) step(entries []fullRoundFlatEntry, spent func(string) bool) {
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
			s.cur.advance(nextFrom)
		}
		s.pos = i
		s.from = i + 1
		s.has = true
		return
	}
	if leading {
		s.cur.advance(nextFrom)
	}
	s.has = false
}

func newPoolIndexStream(
	ctx context.Context,
	ix *fullRoundFlatIndex,
	bucket *fullRoundFlatBucket,
	wild *fullRoundFlatCursor,
	spent func(string) bool,
	n learnNoise,
	livePrint uint64,
	hasLivePrint bool,
) (*poolIndexStream, error) {
	st := &poolIndexStream{entries: ix.entries, spent: spent, curIdx: -1}
	addCursor := func(c *fullRoundFlatCursor) {
		if c == nil || len(c.idx) == 0 {
			return
		}
		start := int(c.next.Load())
		src := poolIndexSource{cur: c, idxs: c.idx, start: start, from: start}
		src.step(ix.entries, spent)
		if src.has {
			st.sources[st.nsrc] = src
			st.nsrc++
		}
	}
	if bucket != nil {
		if len(bucket.all.idx) > 1 && hasLivePrint {
			pm, err := bucket.file(ctx, ix.entries, n)
			if err != nil {
				return nil, err
			}
			addCursor(pm.byPrint[livePrint])
			addCursor(pm.rest)
		} else {
			addCursor(&bucket.all)
		}
	}
	addCursor(wild)
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
	poolIx *fullRoundFlatIndex,
	poolBucket *fullRoundFlatBucket,
	poolWild *fullRoundFlatCursor,
	meta integrations.RoundMeta,
	mockDb integrations.MockMemDb,
	live httpParts,
	liveShape learn.Shape,
	n learnNoise,
	opts models.OutgoingOptions,
	livePrint uint64,
	hasLivePrint bool,
) (*models.Mock, learn.Shape, httpParts, bool, []*models.Mock, bool, error) {
	st, err := newPoolIndexStream(ctx, poolIx, poolBucket, poolWild, mockSpentOf(mockDb), n, livePrint, hasLivePrint)
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
