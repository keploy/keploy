package record

import (
	"bytes"
	"context"
	"crypto/sha256"
	"regexp"
	"sort"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Roller lets the program that embeds the recorder roll a long recording over
// to new test sets, so no test set grows past a size its replay can hold.
//
// A replay works on a whole test set at a time: it reads the set's mock file,
// its mappings and its tests. A recording that goes on for hours into one set
// makes each replay of it bigger than the last (k8s-proxy replays its live
// recording every few minutes, and ran out of memory at 188k mocks). With a
// Roller the recording continues in a new set once its set holds Limit bytes,
// and every set is complete on its own: its tests, every mock its tests'
// mappings name, and the state a test of it needs from before the set began
// (see setRoll).
type Roller interface {
	// Limit is how many bytes of mock documents and mapping entries a test
	// set takes before the recording continues in a new set. 0 or less: the
	// recording stays in one set.
	Limit() int64
	// Next names the test set the recording continues in after cur, made
	// ready to be written (its directory, and wherever the embedder lists
	// the recording's sets).
	Next(ctx context.Context, cur string) (string, error)
	// Rolled says that what is captured after cutAt is written to next, and
	// what was captured up to cutAt stays in prev.
	Rolled(prev, next string, cutAt time.Time)
}

// MappingLimiter is an optional interface a Roller may implement to also roll
// a test set over before its estimated mapping bytes pass a budget (for
// example, a store's single-document size limit). 0 or less disables the
// separate mapping bound.
type MappingLimiter interface {
	MappingLimit() int64
}

// SetRoller rolls the recordings Start makes over to new test sets as ro says
// (Roller). nil, the default, keeps a recording in one set.
func (r *Recorder) SetRoller(ro Roller) { r.roller = ro }

// mockDocStore is what a rollover needs of the mock store: documents of one
// set copied into another, byte for byte (mockdb.MockYaml).
type mockDocStore interface {
	ReadMockDocs(ctx context.Context, testSetID string, names map[string]struct{}) (map[string][]byte, string, error)
	AppendMockDocs(ctx context.Context, testSetID, format string, docs [][]byte) (int64, error)
}

// mappingEntryBytes is about what one mapping entry adds to a set's mapping
// file: its fields' values and the YAML around them.
func mappingEntryBytes(e models.MockEntry) int64 {
	return int64(len(e.Name)+len(e.Kind)+len(e.ReqTimestampMock)+len(e.ResTimestampMock)) + 128
}

// mockMappingEntryBytes estimates what one recorded mock adds to the set's
// mapping document once mapped (its name, kind, RFC3339Nano timestamps, and
// field overhead).
func mockMappingEntryBytes(m *models.Mock) int64 {
	return int64(len(m.Name)+len(m.Kind)) + 60 + 128
}

// maxCarryState caps non-mark, non-startup session/connection state mocks
// carried into subsequent sets so long recordings with heavy connection churn
// stay bounded in memory and per-set size.
const maxCarryState = 512

// rollSet is one test set of a recording. The captures stamped after after
// (and not after the next set's after) are its: a test by its request's
// time, a mock by its request's. The first set's after is the zero time.
type rollSet struct {
	id    string
	idx   int
	after time.Time
	// names is every mock the set's mock file holds (its own and the copies
	// put in it), kept while the set is one of the two newest. nil after: a
	// question about an older set reads its file (setRoll.held).
	names map[string]struct{}
	// tests are the tests routed to the set, forgotten with names.
	tests []string
	// times is each mock routed to the set by its own time, in the order
	// they came, kept with names: what a test in flight at the cut before
	// the set made after it (setRoll.straddled).
	times []timedName
	// maxResp is the latest response time among tests routed to this set:
	// when cut() commits the next set at cutAt, any test routed here with
	// resp > cutAt (including tests that arrived ahead of cut() or during
	// Roller.Next()) is promoted into sr.straddles under sr.mu.
	maxResp time.Time
	// copyMu serializes cross-set mock appends into this set so concurrent
	// wrote(), straddled(), and place() calls never write a mock twice.
	// Lock order is always copyMu -> sr.mu.
	copyMu sync.Mutex
	// windows and marks are the set's own test windows and worker marks
	// (the recorder's, per set).
	windows *windowMappings
	marks   *workerMarks
}

// timedName is a mock's name and its request's time.
type timedName struct {
	at   time.Time
	name string
}

// straddle is a test of set that was in flight when the recording rolled over
// past it: the calls it made after the cut (from) up to its response (to)
// are its set's too, filed by their time in the set after.
type straddle struct {
	set      *rollSet
	from, to time.Time
}

// carried is a mock the next set starts with: session- or connection-lifetime
// state, a mock a worker-mock rule marked, or one the application made before
// the recording's first test (its startup). Replayed on its own, a set's
// application starts from nothing, and makes these calls again.
type carried struct {
	name     string
	at       time.Time
	doc      []byte
	mapBytes int64
	startup  bool
	window   *models.WindowedMock
	mark     *markedCall
}

// setRoll is the recording's test sets once it may roll over (Roller): which
// set each capture goes to, when the current set is full, and the copies that
// keep every set whole.
//
// Sets are spans of time. At the cut T (the latest capture time seen when the
// current set reached the limit) a new set begins: a test goes to the set
// whose span holds its request time, and a mock to the set whose span holds
// its own request's -- so a test still in flight at T, and the mocks it made
// before T, go to the set before, wherever they arrive. A mock a test's
// mapping names that is not in the test's set (made after T by a test from
// before it, or before T by one after it) is copied into the test's set
// before the mapping is written (place). And the new set starts with copies of
// the carried mocks (carried). No mapping names a mock of another set.
type setRoll struct {
	logger   *zap.Logger
	roller   Roller
	store    mockDocStore
	limit    int64
	mapLimit int64
	// newWindows and newMarks make a set's own windows and worker marks.
	newWindows func() *windowMappings
	newMarks   func() *workerMarks
	// streamed reports whether the agent has streamed mappings itself, so
	// aged sets' windowMappings can be freed on rollover.
	streamed func() bool
	// configure writes a new set's config the way Start writes the first
	// set's (createConfigWithMetadata).
	configure func(ctx context.Context, id string)

	mu    sync.Mutex
	sets  []*rollSet
	tests map[string]int
	// bytes is what the newest set holds; carryAt is what it began with.
	// wroteMapBytes / mappedMapBytes track estimated mapping bytes in the
	// newest set for MappingLimiter.
	bytes, carryAt                   int64
	wroteMapBytes, carryWroteMapAt   int64
	mappedMapBytes, carryMappedMapAt int64
	carry                            []carried
	carryNames                       map[string]struct{}
	// carryState is where in carry the state mock of each content is
	// (stateKey): the same exchange recorded again (a connection's ping, a
	// session's setting read again) is carried once, its latest recording.
	carryState map[[sha256.Size]byte]int
	format     string
	// startup is the mocks made before the recording's first test (without
	// their documents), until the first cut (startupOpen) reads them back and
	// carries them.
	startup     []carried
	startupOpen bool
	firstTest   time.Time
	latest      time.Time
	// straddles are the tests in flight at a cut whose later calls are
	// copied into their set as they are written (straddled).
	straddles []straddle
	// cuts is how many times the recording rolled over.
	cuts int
	// wake is signalled when a test is routed, for the mappings parked
	// until their test is.
	wake chan struct{}
}

// newSetRoll returns the sets of a recording that begins in first. It rolls
// over only when ro gives a limit and store can copy mocks; otherwise every
// capture goes to first, as before rollovers.
func newSetRoll(logger *zap.Logger, ro Roller, store any, first string, windows *windowMappings, marks *workerMarks) *setRoll {
	sr := &setRoll{
		logger:      logger,
		tests:       map[string]int{},
		carryNames:  map[string]struct{}{},
		carryState:  map[[sha256.Size]byte]int{},
		startupOpen: true,
		wake:        make(chan struct{}, 1),
	}
	sr.sets = []*rollSet{{id: first, names: map[string]struct{}{}, windows: windows, marks: marks}}
	if ro == nil {
		return sr
	}
	limit := ro.Limit()
	if limit <= 0 {
		return sr
	}
	docs, ok := store.(mockDocStore)
	if !ok {
		logger.Warn("this recording stays in one test set however large it grows: its mock store cannot copy mocks between test sets, which a rollover needs",
			zap.String("testSetID", first), zap.Int64("setLimitBytes", limit))
		return sr
	}
	sr.roller, sr.store, sr.limit = ro, docs, limit
	if ml, ok := ro.(MappingLimiter); ok {
		if mlLimit := ml.MappingLimit(); mlLimit > 0 {
			sr.mapLimit = mlLimit
		}
	}
	return sr
}

// rolls reports whether the recording may roll over.
func (sr *setRoll) rolls() bool { return sr.roller != nil }

// first is the recording's first set.
func (sr *setRoll) first() *rollSet {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.sets[0]
}

// all is every set of the recording, oldest first.
func (sr *setRoll) all() []*rollSet {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return append([]*rollSet(nil), sr.sets...)
}

// last is the newest set.
func (sr *setRoll) last() *rollSet {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.sets[len(sr.sets)-1]
}

// setForLocked is the set whose span holds t; the newest for a zero time.
func (sr *setRoll) setForLocked(t time.Time) *rollSet {
	if t.IsZero() {
		return sr.sets[len(sr.sets)-1]
	}
	for i := len(sr.sets) - 1; i > 0; i-- {
		if t.After(sr.sets[i].after) {
			return sr.sets[i]
		}
	}
	return sr.sets[0]
}

func (sr *setRoll) seeLocked(t time.Time) {
	if t.After(sr.latest) {
		sr.latest = t
	}
}

// test routes a test case, by its request's time, and remembers its set for
// its mapping. It is called for every test case that arrives, kept or not.
func (sr *setRoll) test(tc *models.TestCase) *rollSet {
	req, resp := models.TestCaseWindow(tc)
	sr.mu.Lock()
	sr.seeLocked(req)
	if !req.IsZero() && (sr.firstTest.IsZero() || req.Before(sr.firstTest)) {
		sr.firstTest = req
	}
	s := sr.setForLocked(req)
	if resp.After(s.maxResp) {
		s.maxResp = resp
	}
	sr.registerLocked(tc.Name, s)
	sr.mu.Unlock()
	sr.signal()
	return s
}

func (sr *setRoll) addStraddleLocked(s *rollSet, from, to time.Time) {
	for i := range sr.straddles {
		if sr.straddles[i].set == s && sr.straddles[i].from.Equal(from) {
			if to.After(sr.straddles[i].to) {
				sr.straddles[i].to = to
			}
			return
		}
	}
	sr.straddles = append(sr.straddles, straddle{set: s, from: from, to: to})
}

// registerLocked remembers that test name's mapping goes to s.
func (sr *setRoll) registerLocked(name string, s *rollSet) {
	if name == "" || !sr.rolls() {
		return
	}
	if i, known := sr.tests[name]; !known || i != s.idx {
		s.tests = append(s.tests, name)
	}
	sr.tests[name] = s.idx
}

func (sr *setRoll) signal() {
	if !sr.rolls() {
		return
	}
	select {
	case sr.wake <- struct{}{}:
	default:
	}
}

// saved remembers the name tc was saved under in s, when the store named it
// (a test case that came with no name, or another): a mapping by that name
// goes to s.
func (sr *setRoll) saved(tc *models.TestCase, s *rollSet) {
	if !sr.rolls() || tc.Name == "" {
		return
	}
	sr.mu.Lock()
	sr.registerLocked(tc.Name, s)
	sr.mu.Unlock()
	sr.signal()
}

// cutCount is how many times the recording rolled over.
func (sr *setRoll) cutCount() int {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.cuts
}

// straddled makes s hold what tc made after a cut it was in flight at: tc went
// to s by its request's time, and every mock of a later set from the cut up to
// tc's response is copied into s -- those written already, and (as a
// straddle) those still to come. A replay that serves s's tests by their
// windows finds tc's calls in s, as it would in a recording kept in one set.
func (sr *setRoll) straddled(ctx context.Context, tc *models.TestCase, s *rollSet) {
	if !sr.rolls() {
		return
	}
	_, resp := models.TestCaseWindow(tc)
	if resp.IsZero() {
		return
	}
	sr.mu.Lock()
	if s.idx+1 >= len(sr.sets) || !resp.After(sr.sets[s.idx+1].after) {
		sr.mu.Unlock()
		return
	}
	from := sr.sets[s.idx+1].after
	sr.addStraddleLocked(s, from, resp)
	src := map[*rollSet]map[string]struct{}{}
	for _, n := range sr.sets[s.idx+1:] {
		if !resp.After(n.after) {
			break
		}
		for _, t := range n.times {
			if t.at.After(from) && !t.at.After(resp) {
				if _, there := s.names[t.name]; s.names == nil || !there {
					if src[n] == nil {
						src[n] = map[string]struct{}{}
					}
					src[n][t.name] = struct{}{}
				}
			}
		}
	}
	format := sr.format
	sr.mu.Unlock()
	for n, names := range src {
		got, gotFormat, err := sr.store.ReadMockDocs(ctx, n.id, names)
		if err != nil {
			utils.LogError(sr.logger, err, "keploy could not read the calls a test in flight at the cut made after it",
				zap.String("testSetID", s.id), zap.String("fromTestSetID", n.id), zap.String("test", tc.Name))
			continue
		}
		if gotFormat != "" {
			format = gotFormat
		}
		sr.copyIn(ctx, s, format, got)
	}
}

// copyIn appends docs (name -> document) to s's mock file and notes them in s,
// skipping any mock s already holds (serialized per set by s.copyMu).
func (sr *setRoll) copyIn(ctx context.Context, s *rollSet, format string, docs map[string][]byte) {
	if len(docs) == 0 {
		return
	}
	names := make([]string, 0, len(docs))
	for n := range docs {
		names = append(names, n)
	}
	sort.Strings(names)
	_, err := sr.copyInOrdered(ctx, s, format, names, docs)
	if err != nil {
		utils.LogError(sr.logger, err, "keploy could not copy into a test set the calls a test of it in flight at the cut made after it",
			zap.String("testSetID", s.id), zap.Int("mocks", len(names)))
	}
}

// copyInOrdered appends docs for orderedNames into s under s.copyMu, filtering
// out any mock already present in s.names (or on disk when s has aged out).
// Returns the names that were newly appended (or already present) and any
// append error.
func (sr *setRoll) copyInOrdered(ctx context.Context, s *rollSet, format string, orderedNames []string, docs map[string][]byte) ([]string, error) {
	s.copyMu.Lock()
	defer s.copyMu.Unlock()

	sr.mu.Lock()
	open := s.names != nil
	want := make([]string, 0, len(orderedNames))
	for _, nm := range orderedNames {
		if _, ok := docs[nm]; !ok {
			continue
		}
		if open {
			if _, have := s.names[nm]; have {
				continue
			}
		}
		want = append(want, nm)
	}
	sr.mu.Unlock()
	if len(want) == 0 {
		return nil, nil
	}
	if !open {
		need := make(map[string]struct{}, len(want))
		for _, nm := range want {
			need[nm] = struct{}{}
		}
		have, _, err := sr.store.ReadMockDocs(ctx, s.id, need)
		if err == nil && len(have) > 0 {
			filtered := want[:0]
			for _, nm := range want {
				if _, exists := have[nm]; !exists {
					filtered = append(filtered, nm)
				}
			}
			want = filtered
		}
		if len(want) == 0 {
			return nil, nil
		}
	}
	list := make([][]byte, 0, len(want))
	for _, nm := range want {
		list = append(list, docs[nm])
	}
	n, err := sr.store.AppendMockDocs(ctx, s.id, format, list)
	if err != nil {
		return nil, err
	}
	sr.mu.Lock()
	if s.names != nil {
		for _, nm := range want {
			s.names[nm] = struct{}{}
		}
	}
	if s == sr.sets[len(sr.sets)-1] {
		sr.bytes += n
		for _, nm := range want {
			sr.wroteMapBytes += int64(len(nm)) + 194
		}
	}
	sr.mu.Unlock()
	return want, nil
}

// setOfTest is the set a test's mapping goes to: the set its test case went
// to. ok is false for a test not seen yet after a cut, whose mapping waits
// for it (before any cut there is one set, and it is that one).
func (sr *setRoll) setOfTest(name string) (*rollSet, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if len(sr.sets) == 1 {
		return sr.sets[0], true
	}
	if i, ok := sr.tests[name]; ok {
		return sr.sets[i], true
	}
	return nil, false
}

// mock routes a mock, by its request's time.
func (sr *setRoll) mock(m *models.Mock) *rollSet {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.setForLocked(m.Spec.ReqTimestampMock)
}

// wrote counts a mock written to s, doc its document (format "yaml" or
// "json"; nil when the store handed none), and keeps it to carry when the
// next set needs it (carried).
func (sr *setRoll) wrote(ctx context.Context, s *rollSet, m *models.Mock, doc []byte, format string, mark *markedCall, window *models.WindowedMock) {
	if !sr.rolls() || m.Name == "" {
		return
	}
	at := m.Spec.ReqTimestampMock
	state := m.RecordedAsState()
	mapBytes := mockMappingEntryBytes(m)
	// A test in flight at a cut before s owns this call too when it falls
	// in its window: it is copied into the test's set as it is written.
	var into []*rollSet
	defer func() {
		for _, t := range into {
			sr.copyIn(ctx, t, format, map[string][]byte{m.Name: doc})
		}
	}()
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if doc != nil && !at.IsZero() {
		for _, st := range sr.straddles {
			if st.set.idx < s.idx && at.After(st.from) && !at.After(st.to) && !containsSet(into, st.set) {
				if st.set.names != nil {
					if _, have := st.set.names[m.Name]; have {
						continue
					}
				}
				into = append(into, st.set)
			}
		}
	}
	if s.times != nil || s.names != nil {
		s.times = append(s.times, timedName{at: at, name: m.Name})
	}
	sr.seeLocked(at)
	if sr.format == "" {
		sr.format = format
	}
	if s.names != nil {
		s.names[m.Name] = struct{}{}
	}
	if s == sr.sets[len(sr.sets)-1] {
		sr.bytes += int64(len(doc))
		sr.wroteMapBytes += mapBytes
	}
	if doc == nil {
		return
	}
	if state || mark != nil {
		if _, dup := sr.carryNames[m.Name]; dup {
			return
		}
		c := carried{name: m.Name, at: at, doc: doc, mapBytes: mapBytes, window: window, mark: mark}
		sr.carryNames[m.Name] = struct{}{}
		if mark == nil {
			key := stateKey(doc)
			if i, again := sr.carryState[key]; again {
				delete(sr.carryNames, sr.carry[i].name)
				sr.carry[i] = c
				return
			}
			sr.carryState[key] = len(sr.carry)
		}
		sr.carry = append(sr.carry, c)
		if mark == nil && len(sr.carryState) > maxCarryState {
			sr.evictOldestStateLocked()
		}
		return
	}
	// The application's startup: what it did before its first test, which
	// every set's replay starts with. Named until the first cut, as long as
	// no test seen yet came before it; the cut reads the documents back.
	if sr.startupOpen && !at.IsZero() && (sr.firstTest.IsZero() || at.Before(sr.firstTest)) {
		sr.startup = append(sr.startup, carried{name: m.Name, at: at, mapBytes: mapBytes, startup: true, window: window})
	}
}

// evictOldestStateLocked removes the oldest non-mark, non-startup state entry
// from sr.carry to bound carry size under long-running connection churn.
func (sr *setRoll) evictOldestStateLocked() {
	for i, c := range sr.carry {
		if c.mark != nil || c.startup {
			continue
		}
		delete(sr.carryNames, c.name)
		sr.carry = append(sr.carry[:i], sr.carry[i+1:]...)
		clear(sr.carryState)
		for j, item := range sr.carry {
			if item.mark == nil && !item.startup {
				sr.carryState[stateKey(item.doc)] = j
			}
		}
		return
	}
}

// mapped counts the mapping entries written to s.
func (sr *setRoll) mapped(s *rollSet, entries []models.MockEntry) {
	if !sr.rolls() {
		return
	}
	var n int64
	for _, e := range entries {
		n += mappingEntryBytes(e)
	}
	sr.mu.Lock()
	if s == sr.sets[len(sr.sets)-1] {
		sr.bytes += n
		sr.mappedMapBytes += n
	}
	sr.mu.Unlock()
}

// due reports whether the newest set is full: it holds the byte limit (or
// optional mapping limit), at least half of it its own (a set that begins with
// a large carry still takes half a limit of new captures before it rolls,
// rather than rolling at once).
func (sr *setRoll) due() bool {
	if !sr.rolls() {
		return false
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	byBytes := sr.bytes >= sr.limit && sr.bytes-sr.carryAt >= sr.limit/2
	byMap := sr.mapLimit > 0 &&
		((sr.wroteMapBytes >= sr.mapLimit && sr.wroteMapBytes-sr.carryWroteMapAt >= sr.mapLimit/2) ||
			(sr.mappedMapBytes >= sr.mapLimit && sr.mappedMapBytes-sr.carryMappedMapAt >= sr.mapLimit/2))
	return byBytes || byMap
}

// cut begins the next set: it names it (Roller.Next), copies the carried mocks
// into it, and from then on routes what is captured after the cut to it. A
// failure leaves the recording in the set it is in, to try again once the set
// has taken another half limit.
func (sr *setRoll) cut(ctx context.Context) {
	sr.mu.Lock()
	cur := sr.sets[len(sr.sets)-1]
	at := sr.latest
	if at.IsZero() || !at.After(cur.after) {
		at = time.Now()
	}
	var startup []carried
	startupHadFirstTest := !sr.firstTest.IsZero()
	if sr.startupOpen && startupHadFirstTest {
		for _, c := range sr.startup {
			if _, dup := sr.carryNames[c.name]; !dup && c.at.Before(sr.firstTest) {
				startup = append(startup, c)
			}
		}
		if len(startup) == 0 {
			sr.startupOpen = false
			sr.startup = nil
		}
	}
	format := sr.format
	limit := sr.limit
	sr.mu.Unlock()

	// The startup's documents, read back from the first set, where they are
	// near the front. Keep sr.startupOpen / sr.startup intact on a transient
	// read error so a later cut can retry.
	if len(startup) > 0 {
		want := make(map[string]struct{}, len(startup))
		for _, c := range startup {
			want[c.name] = struct{}{}
		}
		got, _, err := sr.store.ReadMockDocs(context.WithoutCancel(ctx), sr.first().id, want)
		if err != nil {
			utils.LogError(sr.logger, err, "keploy could not read back the calls the application made before the recording's first test; will retry on the next rollover",
				zap.String("testSetID", sr.first().id), zap.Int("mocks", len(want)))
		} else {
			sr.mu.Lock()
			for _, c := range startup {
				if doc, ok := got[c.name]; ok {
					if _, dup := sr.carryNames[c.name]; !dup {
						c.doc = doc
						sr.carryNames[c.name] = struct{}{}
						sr.carry = append(sr.carry, c)
					}
				}
			}
			sr.startupOpen = false
			sr.startup = nil
			sr.mu.Unlock()
		}
	}
	sr.mu.Lock()
	carry := append([]carried(nil), sr.carry...)
	sr.mu.Unlock()

	next, err := sr.roller.Next(ctx, cur.id)
	if err == nil && next == cur.id {
		err = errSameSet
	}
	if err != nil {
		sr.giveUp(cur.id, "the next test set could not be made", err)
		return
	}
	if sr.configure != nil {
		sr.configure(ctx, next)
	}
	docs := make([][]byte, 0, len(carry))
	names := make(map[string]struct{}, len(carry))
	var carryMap int64
	for _, c := range carry {
		docs = append(docs, c.doc)
		names[c.name] = struct{}{}
		if c.mapBytes > 0 {
			carryMap += c.mapBytes
		} else {
			carryMap += int64(len(c.name)) + 194
		}
	}
	n, err := sr.store.AppendMockDocs(context.WithoutCancel(ctx), next, format, docs)
	if err != nil {
		sr.giveUp(cur.id, "the mocks the next test set starts with could not be written to it", err)
		return
	}
	s := &rollSet{id: next, after: at, names: names}
	if sr.newWindows != nil {
		s.windows = sr.newWindows()
	}
	if sr.newMarks != nil {
		s.marks = sr.newMarks()
	}
	var marks []markedCall
	for _, c := range carry {
		if c.window != nil {
			s.windows.addWindowed(*c.window)
		}
		if c.mark != nil {
			marks = append(marks, *c.mark)
		}
	}
	s.marks.adopt(marks)

	sr.mu.Lock()
	s.idx = len(sr.sets)
	sr.sets = append(sr.sets, s)
	if cur.maxResp.After(at) {
		sr.addStraddleLocked(cur, at, cur.maxResp)
	}
	sr.cuts++
	sr.bytes, sr.carryAt = n, n
	sr.wroteMapBytes, sr.carryWroteMapAt = carryMap, carryMap
	sr.mappedMapBytes, sr.carryMappedMapAt = 0, 0
	// The set before the previous one is no longer written to by the
	// captures: what is asked of it from now on is read from its file.
	var agedWindows *windowMappings
	if old := len(sr.sets) - 3; old >= 0 {
		o := sr.sets[old]
		o.names = nil
		o.times = nil
		if sr.streamed != nil && sr.streamed() {
			agedWindows = o.windows
		}
		kept := sr.straddles[:0]
		for _, st := range sr.straddles {
			if st.set.idx > old {
				kept = append(kept, st)
			}
		}
		sr.straddles = kept
		for _, t := range o.tests {
			if sr.tests[t] == old {
				delete(sr.tests, t)
			}
		}
		o.tests = nil
	}
	sr.mu.Unlock()
	if agedWindows != nil {
		agedWindows.clear()
	}

	sr.roller.Rolled(cur.id, next, at)
	sr.logger.Info("the recording continues in a new test set: the one it was in reached its size limit",
		zap.String("testSetID", cur.id), zap.String("nextTestSetID", next),
		zap.Int64("setLimitBytes", limit), zap.Time("cutAt", at),
		zap.Int("carriedMocks", len(carry)), zap.Int64("carriedBytes", n))
}

var errSameSet = errorString("the embedder named the same test set to continue in")

type errorString string

func (e errorString) Error() string { return string(e) }

func (sr *setRoll) giveUp(id, why string, err error) {
	sr.mu.Lock()
	sr.carryAt = sr.bytes
	sr.carryWroteMapAt = sr.wroteMapBytes
	sr.carryMappedMapAt = sr.mappedMapBytes
	sr.mu.Unlock()
	utils.LogError(sr.logger, err, "the recording stays in its test set past the size limit for now: "+why+"; it tries again once the set has taken another half of the limit",
		zap.String("testSetID", id),
		zap.String("next_step", "fix the error above"))
}

// heldBy is the set among the two newest whose mock file holds name, nil when
// neither does.
func (sr *setRoll) heldByLocked(name string) *rollSet {
	for i := len(sr.sets) - 1; i >= 0 && i >= len(sr.sets)-2; i-- {
		if _, ok := sr.sets[i].names[name]; ok {
			return sr.sets[i]
		}
	}
	return nil
}

// place makes every mock entries names a mock of s's mock file, copying from
// the set that holds it each one s does not hold. It returns the entries
// whose mock it could find in no set, which the mapping keeps (a replay
// reports them as missing) and the log names.
func (sr *setRoll) place(ctx context.Context, s *rollSet, entries []models.MockEntry) (missing []string) {
	if !sr.rolls() || len(entries) == 0 {
		return nil
	}
	sr.mu.Lock()
	if len(sr.sets) == 1 {
		sr.mu.Unlock()
		return nil
	}
	// from: source set -> names to copy out of it; unknown: names no open
	// set's index places.
	from := map[*rollSet]map[string]struct{}{}
	var unknown []models.MockEntry
	need := map[string]struct{}{}
	for _, e := range entries {
		if s.names != nil {
			if _, ok := s.names[e.Name]; ok {
				continue
			}
		}
		if _, dup := need[e.Name]; dup {
			continue
		}
		need[e.Name] = struct{}{}
		if h := sr.heldByLocked(e.Name); h != nil {
			if h == s {
				delete(need, e.Name)
				continue
			}
			if from[h] == nil {
				from[h] = map[string]struct{}{}
			}
			from[h][e.Name] = struct{}{}
			continue
		}
		unknown = append(unknown, e)
	}
	open := s.names != nil
	format := sr.format
	sr.mu.Unlock()
	if len(need) == 0 {
		return nil
	}
	// A set older than the two newest has no index: what it holds already
	// is read from its file.
	if !open {
		have, _, err := sr.store.ReadMockDocs(ctx, s.id, need)
		if err == nil {
			for n := range have {
				delete(need, n)
				for _, src := range from {
					delete(src, n)
				}
			}
			kept := unknown[:0]
			for _, e := range unknown {
				if _, there := have[e.Name]; !there {
					kept = append(kept, e)
				}
			}
			unknown = kept
		}
	}
	// A mock no open set's index places is looked for in the set its time
	// says.
	for _, e := range unknown {
		sr.mu.Lock()
		h := sr.setForLocked(mockEntryAt(e))
		sr.mu.Unlock()
		if h == s {
			missing = append(missing, e.Name)
			continue
		}
		if from[h] == nil {
			from[h] = map[string]struct{}{}
		}
		from[h][e.Name] = struct{}{}
	}
	srcs := make([]*rollSet, 0, len(from))
	for h := range from {
		srcs = append(srcs, h)
	}
	sort.Slice(srcs, func(i, j int) bool { return srcs[i].idx < srcs[j].idx })
	for _, h := range srcs {
		names := from[h]
		if len(names) == 0 {
			continue
		}
		got, gotFormat, err := sr.store.ReadMockDocs(ctx, h.id, names)
		if err != nil {
			utils.LogError(sr.logger, err, "keploy could not read the mocks a test names from the test set that holds them",
				zap.String("testSetID", s.id), zap.String("fromTestSetID", h.id), zap.Int("mocks", len(names)))
			for n := range names {
				missing = append(missing, n)
			}
			continue
		}
		if gotFormat != "" {
			format = gotFormat
		}
		ordered := make([]string, 0, len(got))
		seen := make(map[string]struct{}, len(got))
		for _, e := range entries {
			if _, ok := got[e.Name]; ok {
				if _, dup := seen[e.Name]; !dup {
					seen[e.Name] = struct{}{}
					ordered = append(ordered, e.Name)
				}
			}
		}
		for n := range names {
			if _, ok := seen[n]; !ok {
				missing = append(missing, n)
			}
		}
		if len(ordered) == 0 {
			continue
		}
		copied, err := sr.copyInOrdered(ctx, s, format, ordered, got)
		if err != nil {
			utils.LogError(sr.logger, err, "keploy could not copy the mocks a test names into its test set",
				zap.String("testSetID", s.id), zap.String("fromTestSetID", h.id), zap.Int("mocks", len(ordered)))
			missing = append(missing, ordered...)
			continue
		}
		if len(copied) > 0 {
			sr.logger.Debug("copied the mocks a test names into its test set from the set that holds them",
				zap.String("testSetID", s.id), zap.String("fromTestSetID", h.id), zap.Strings("mocks", copied))
		}
	}
	sort.Strings(missing)
	return missing
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// mockEntryAt is the time of a mapping entry's mock's request (entryAt), or
// of its second when that is all the entry has.
func mockEntryAt(e models.MockEntry) time.Time {
	at := entryAt(e)
	if at.IsZero() && e.Timestamp != 0 {
		at = time.Unix(e.Timestamp, 0)
	}
	return at
}

// groupEntriesByTime groups entries by the set each entry's own timestamp
// falls in, acquiring sr.mu once for the whole batch.
func (sr *setRoll) groupEntriesByTime(entries []models.MockEntry) ([]*rollSet, map[*rollSet][]models.MockEntry) {
	groups := make(map[*rollSet][]models.MockEntry)
	var order []*rollSet
	sr.mu.Lock()
	for _, e := range entries {
		s := sr.setForLocked(mockEntryAt(e))
		if _, ok := groups[s]; !ok {
			order = append(order, s)
		}
		groups[s] = append(groups[s], e)
	}
	sr.mu.Unlock()
	return order, groups
}

// noteStop notes the recording's stop on every set's windows.
func (sr *setRoll) noteStop(at time.Time) {
	for _, s := range sr.all() {
		s.windows.noteStop(at)
	}
}

// insertedDocFor is NewInsertedMockDoc, handed every inserted mock's document
// also when no hook asks for it, when the recording may roll over: a rollover
// counts a set's bytes by them, and carries some of them (setRoll.wrote).
func insertedDocFor(ctx context.Context, hooks RecordHooks, always bool) *InsertedMockDoc {
	d := NewInsertedMockDoc(ctx, hooks)
	if always && d.ctx == ctx {
		d.ctx = models.WithMockDocReceiver(ctx, d.receive)
	}
	return d
}

// begin names the recording's first set, and how each set after it gets its
// own test windows and worker marks.
func (sr *setRoll) begin(first string, newWindows func() *windowMappings, newMarks func() *workerMarks) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.sets[0].id = first
	sr.newWindows, sr.newMarks = newWindows, newMarks
}

// createRolledSetConfig writes the config of a set a recording rolled over to,
// as createConfigWithMetadata writes the first set's: the recording's
// metadata, its name the set's own.
func (r *Recorder) createRolledSetConfig(ctx context.Context, testSetID string) {
	metadata, err := utils.ParseMetadata(r.config.Record.Metadata)
	if err != nil {
		utils.LogError(r.logger, err, "failed to parse metadata", zap.String("metadata", r.config.Record.Metadata))
		return
	}
	if _, named := metadata["name"]; named {
		metadata["name"] = testSetID
	}
	conf := &models.TestSet{Template: make(map[string]interface{}), Metadata: metadata}
	if err := r.testSetConf.Write(ctx, testSetID, conf); err != nil {
		utils.LogError(r.logger, err, "Failed to create test-set config file with metadata", zap.String("testSet", testSetID))
	}
}

// rebalanceWindows gives each set's windows the mocks inside its tests'
// windows, wherever they were filed, and takes from a set's windows a mock
// that is inside another set's test's window and none of its own: a mapping
// derived from a set's windows (windowMappings.writeFor) then names what its
// tests made, the copies place puts in it before it is written. It is for a
// recording the agent did not map, at its end.
func (sr *setRoll) rebalanceWindows() {
	sets := sr.all()
	// Per set, its tests' windows by start, and the latest end among the
	// windows up to each: a time is inside one of them when the latest end of
	// those that start at or before it is not before it.
	type spans struct{ starts, maxEnds []time.Time }
	of := make([]spans, len(sets))
	for i, s := range sets {
		if s.windows == nil {
			continue
		}
		type win struct{ start, end time.Time }
		var ws []win
		s.windows.mu.Lock()
		for _, t := range s.windows.tests {
			if keptWindow(t.TestCase) && !t.Start.IsZero() && !t.End.IsZero() {
				ws = append(ws, win{t.Start, t.End})
			}
		}
		s.windows.mu.Unlock()
		sort.Slice(ws, func(a, b int) bool { return ws[a].start.Before(ws[b].start) })
		var latest time.Time
		for _, w := range ws {
			if w.end.After(latest) {
				latest = w.end
			}
			of[i].starts = append(of[i].starts, w.start)
			of[i].maxEnds = append(of[i].maxEnds, latest)
		}
	}
	inside := func(i int, at time.Time) bool {
		k := sort.Search(len(of[i].starts), func(k int) bool { return of[i].starts[k].After(at) }) - 1
		return k >= 0 && !of[i].maxEnds[k].Before(at)
	}
	add := make([][]models.WindowedMock, len(sets))
	for i, s := range sets {
		if s.windows == nil {
			continue
		}
		s.windows.mu.Lock()
		kept := s.windows.mocks[:0]
		for _, m := range s.windows.mocks {
			own := inside(i, m.At)
			elsewhere := false
			for j := range sets {
				if j != i && sets[j].windows != nil && inside(j, m.At) {
					add[j] = append(add[j], m)
					elsewhere = true
				}
			}
			if own || !elsewhere {
				kept = append(kept, m)
			}
		}
		s.windows.mocks = kept
		s.windows.mu.Unlock()
	}
	for j, s := range sets {
		if s.windows == nil || len(add[j]) == 0 {
			continue
		}
		s.windows.mu.Lock()
		have := make(map[string]struct{}, len(s.windows.mocks))
		for _, m := range s.windows.mocks {
			have[m.Name] = struct{}{}
		}
		for _, m := range add[j] {
			if _, ok := have[m.Name]; !ok {
				have[m.Name] = struct{}{}
				s.windows.mocks = append(s.windows.mocks, m)
			}
		}
		sort.SliceStable(s.windows.mocks, func(a, b int) bool { return s.windows.mocks[a].At.Before(s.windows.mocks[b].At) })
		s.windows.mu.Unlock()
	}
}

// stateVaries is what of a mock document changes between two recordings of
// the same exchange: its times ("timestamp: t", "reqTimestampMock: t", in YAML
// or JSON) and its own name, the top-level one: in YAML the line "name: x" at
// the left margin, in JSON the first "name" (keploy writes version, kind and
// name first). A name inside the spec (a column's, a header's) is the
// exchange's, and stays.
var (
	stateTimes    = regexp.MustCompile(`(?m)^ *(timestamp|reqTimestampMock|resTimestampMock): .*$|"(timestamp|reqTimestampMock|resTimestampMock)":"[^"]*"`)
	stateYAMLName = regexp.MustCompile(`(?m)^name: .*$`)
	stateJSONName = regexp.MustCompile(`"name":"(?:[^"\\]|\\.)*"`)
)

// stateKey is a state mock's document less what changes between two
// recordings of the same exchange: two with one key are the same request and
// answer on the same connection (its id is in the document), which a replay
// serves from one as well as from both.
func stateKey(doc []byte) [sha256.Size]byte {
	d := bytes.TrimSpace(doc)
	if bytes.HasPrefix(d, []byte("{")) {
		if loc := stateJSONName.FindIndex(d); loc != nil {
			d = append(append([]byte(nil), d[:loc[0]]...), d[loc[1]:]...)
		}
	} else {
		d = stateYAMLName.ReplaceAll(d, nil)
	}
	return sha256.Sum256(stateTimes.ReplaceAll(d, nil))
}

func containsSet(list []*rollSet, s *rollSet) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
