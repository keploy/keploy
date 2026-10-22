package record

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/replay"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// windowMappings collects, while a recording made without --sync runs under
// the proven switch, what its mappings.yaml is derived from at the end: the
// window of every test case it persisted and every mock it persisted (design
// v14 row S3). Without --sync the agent maps no test to its mocks, so without
// this the set has no mappings.yaml and its first replay made one from what
// each test happened to consume: a mock a test did not consume then dropped
// out of its mapping for good, and no later replay could see it go unconsumed.
//
// The windows are derived once, at the end (models.DeriveWindowMappings): a
// mock is often handed over before the test case of the window it is in, and a
// long request's window can contain mocks handed over long before it ends.
type windowMappings struct {
	mu    sync.Mutex
	tests []models.TestWindow
	// lost is how many windows of test cases the recording failed to persist
	// tests holds (see addLostTest).
	lost int
	// mocks holds only the test mocks: a protocol-phase or async-lane mock
	// is no test's, and a recording can make millions of them.
	mocks []models.WindowedMock
	// stopAt is when keploy record began stopping the application (a stop
	// signal, the record timer), zero while it has not: a worker-scope mock
	// recorded from then on is the application's shutdown's
	// (MockEntry.AtStop).
	stopAt time.Time
}

// noteStop records that keploy record began stopping the application at at;
// the earliest such moment is kept.
func (w *windowMappings) noteStop(at time.Time) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.stopAt.IsZero() || at.Before(w.stopAt) {
		w.stopAt = at
	}
	w.mu.Unlock()
}

// markAtStop marks, of the worker scope, each mock placed once keploy record
// had begun stopping the application (stopAt): its shutdown's calls, which a
// replay requires only once it has stopped the application. A mock's place is
// placed's (models.PlaceOf, as the window mappings read it: its answer's time
// for a mock placed at its answer), else its request time.
func markAtStop(worker []models.MockEntry, placed map[string]time.Time, stopAt time.Time) {
	if stopAt.IsZero() {
		return
	}
	for i := range worker {
		at, ok := placed[worker[i].Name]
		if !ok {
			var err error
			if at, err = models.ParseMockTimestamp(worker[i].ReqTimestampMock); err != nil {
				continue
			}
		}
		if !at.IsZero() && !at.Before(stopAt) {
			worker[i].AtStop = true
		}
	}
}

// lostTestPrefix starts the window name of a test case the recording failed
// to persist. No test case is named so: a test case's name is a file name.
const lostTestPrefix = "\x00lost-"

// newWindowMappings is the collector of a recording that derives its
// mappings.yaml from its test windows: one made without --sync under the
// proven switch, with mapping not disabled. Any other recording gets nil,
// which collects nothing, so it writes exactly what it wrote before.
//
// Such a recording is not marked for S12's background rules either
// (models.RecordedWithS12Routing): its set is served by today's. Under a
// build that enables them it is said, with how to record one that is.
func (r *Recorder) newWindowMappings() *windowMappings {
	if !provenswitch.On() {
		return nil
	}
	if r.config.Record.Synchronous || r.config.DisableMapping {
		if replay.S12Serving() && r.logger != nil {
			r.logger.Info("this recording does not route background writes nor mark its test set for S12's serving: a recording made with --sync, or with mapping disabled, derives no test windows, so its set is served by the rules of a set recorded before S12, every recorded background call answered exactly and required by the set's end ("+provenswitch.Env+"=1)",
				zap.Bool("sync", r.config.Record.Synchronous),
				zap.Bool("mappingDisabled", r.config.DisableMapping),
				zap.String("next_step", "record without --sync, with mapping on, for S12's routing and serving"))
		}
		return nil
	}
	return &windowMappings{}
}

// addTest records the window of a test case once it is persisted (under the
// name its insert gave it). One that came back without a name keeps its window
// and gets no entry, as a test case the recording failed to persist does: an
// entry needs the test's name.
func (w *windowMappings) addTest(tc *models.TestCase) {
	if w == nil {
		return
	}
	if tc == nil || tc.Name == "" {
		w.addLostTest(tc)
		return
	}
	win := models.RecordedWindow(tc)
	w.mu.Lock()
	w.tests = append(w.tests, win)
	w.mu.Unlock()
}

// addLostTest records the window of a test case the recording failed to
// persist. It owns what was recorded inside it, as a test case the finalize
// deletes does, so those mocks go to no test rather than to one that ran
// beside it; it gets no entry.
func (w *windowMappings) addLostTest(tc *models.TestCase) {
	if w == nil {
		return
	}
	win := models.RecordedWindow(tc)
	w.mu.Lock()
	w.lost++
	win.TestCase = fmt.Sprintf("%s%d", lostTestPrefix, w.lost)
	w.tests = append(w.tests, win)
	w.mu.Unlock()
}

// addMock records a mock once it is persisted (under the name its insert gave
// it, with the async lane its hooks stamped on it), when a test may own it.
func (w *windowMappings) addMock(m *models.Mock) {
	if w == nil {
		return
	}
	wm := models.NewWindowedMock(m)
	if !wm.TestMock || wm.At.IsZero() {
		return
	}
	w.mu.Lock()
	w.mocks = append(w.mocks, wm)
	w.mu.Unlock()
}

// write derives the set's mappings from its test windows and writes them to
// mappings.yaml: an entry for every test case the recording kept, with the
// test mocks of its window. A test case the finalize deleted (in deleted)
// keeps its window, so the mocks recorded inside it stay its own and are not
// handed to a test that ran beside it; its entry is left out.
//
// streamed is how many mappings the agent handed over. An agent that maps a
// recording made without --sync itself (enterprise's static dedup resolves its
// windows in the agent) has written them already, and entries derived here as
// well could list one call under two tests: none is derived.
//
// drained says the record drain joined every persist goroutine. When it did
// not, a test case or mock may still be on its way to disk, and a mapping
// derived now could leave a test without a mock it recorded: none is written.
// A mapping that is not written, or whose write fails, fails the recording, as
// a failed mappings write refuses under the proven model (design v14 §5.2).
func (w *windowMappings) write(ctx context.Context, logger *zap.Logger, db MappingDb, testSetID string, deleted map[string]struct{}, drained bool, streamed uint64) {
	w.writeFor(ctx, logger, db, testSetID, deleted, drained, streamed, nil)
}

// writeFor is write, telling mapped, when set, each kept test case's mocks as
// its window derives them, as consumeMappingsFor tells a streamed mapping's:
// the capture watch charges a test case with a mock of its whose container
// the Docker daemon did not answer the read of (captureWatch.mapped).
func (w *windowMappings) writeFor(ctx context.Context, logger *zap.Logger, db MappingDb, testSetID string, deleted map[string]struct{}, drained bool, streamed uint64, mapped func(test string, entries []models.MockEntry)) {
	if w == nil {
		return
	}
	if streamed > 0 {
		logger.Info("the keploy agent mapped this recording's tests to their mocks itself, so no mappings are derived from the test windows",
			zap.String("testSetID", testSetID),
			zap.Uint64("mappingsFromTheAgent", streamed))
		// Recorded under S12's serving all the same, routed writes or not:
		// record's judging judges what that serving answers.
		if marker, s12 := s12Record(db); s12 && drained {
			if err := marker.MarkRecordedWith(ctx, testSetID, models.RecordedWithS12Routing); err != nil {
				logger.Error("failed to mark the test set as recorded under S12's routing and serving (mappings.yaml recordedWith)",
					zap.String("testSetID", testSetID),
					zap.Error(err),
					zap.String("next_step", "fix the error above and re-record this test set"))
				utils.SetExitCodeOnce(utils.ExitKeployError)
			}
		}
		return
	}
	if !drained {
		logger.Error("mappings.yaml was not written: the record drain timed out, so a test case or mock may still have been on its way to disk, and a mapping derived from the test windows now could leave a test without a call it recorded",
			zap.String("testSetID", testSetID),
			zap.String("next_step", "re-record this test set"))
		utils.SetExitCodeOnce(utils.ExitKeployError)
		return
	}
	// A set this build records under S12 (s12Record) is marked so, and its
	// in-flight writes are routed (S12 v16 §2.2): only S12's serving serves
	// a routed write by its kind.
	marker, s12 := s12Record(db)
	// The drain joined every goroutine that adds to the collector: derive from
	// it as it is, without a copy.
	w.mu.Lock()
	derived := models.DeriveWindowMappingsWith(w.tests, w.mocks, models.DeriveOptions{RouteInFlightWrites: s12})
	stopAt := w.stopAt
	placed := make(map[string]time.Time, len(w.mocks))
	for _, m := range w.mocks {
		if !m.Place.IsZero() {
			placed[m.Name] = m.Place
		} else {
			placed[m.Name] = m.At
		}
	}
	w.mu.Unlock()
	kept := func(id string) bool {
		_, gone := deleted[id]
		return !gone && !strings.HasPrefix(id, lostTestPrefix)
	}
	conc := keptConcurrencyOf(derived, kept)
	byTest := make(map[string][]models.MockEntry, len(derived.Tests))
	mappedMocks := 0
	for _, tc := range derived.Tests {
		if !kept(tc.ID) {
			continue
		}
		byTest[tc.ID] = tc.Mocks
		mappedMocks += len(tc.Mocks)
		if mapped != nil && len(tc.Mocks) > 0 {
			mapped(tc.ID, tc.Mocks)
		}
	}
	var concurrent []string
	for _, id := range derived.Concurrent {
		if kept(id) {
			concurrent = append(concurrent, id)
		}
	}
	if len(byTest) == 0 {
		return
	}
	if err := db.UpsertBatch(ctx, testSetID, byTest); err != nil {
		logger.Error("failed to save the test-mock mappings derived from the test windows",
			zap.String("testSetID", testSetID),
			zap.Error(err),
			zap.String("next_step", "keploy test cannot tell which recorded calls each test owns without mappings.yaml; fix the error above and re-record this test set"))
		utils.SetExitCodeOnce(utils.ExitKeployError)
		return
	}
	// The set's worker scope (D2): what no test owns, each at its place. A
	// place whose test case the recording did not keep moves back to the
	// latest kept test case before it (the set's start before every one), so
	// a replay of the kept test cases reaches it.
	worker := keptPlaces(derived, kept)
	markAtStop(worker, placed, stopAt)
	routedReasons(logger, testSetID, worker, derived.Routed, kept)
	if err := db.UpsertWorker(ctx, testSetID, worker); err != nil {
		logger.Error("failed to save the test set's worker scope, the calls no test made, derived from the test windows",
			zap.String("testSetID", testSetID),
			zap.Error(err),
			zap.String("next_step", "keploy test cannot tell which recorded calls are the set's own without mappings.yaml's worker scope; fix the error above and re-record this test set"))
		utils.SetExitCodeOnce(utils.ExitKeployError)
		return
	}
	// The concurrency groups (models.Mapping.Groups) and the calls recorded
	// while several tests were open whose first-opened test was not kept
	// (models.Mapping.Shared): keploy record's full round works out which
	// of the kept tests made each. Written whole, as the worker scope is: a
	// recording with none clears what a file it writes into held (a store
	// leaves a file it changes nothing of as it is).
	if cs, ok := db.(concurrencyStore); ok || len(conc.groups) > 0 {
		if !ok {
			logger.Error("mappings.yaml cannot hold which tests were open together while a call was recorded: this keploy's mapping store does not write them, so each such call stays the test whose request came first",
				zap.String("testSetID", testSetID),
				zap.String("next_step", "record the test set again with a keploy whose mapping store writes mappings.yaml's groups"))
		} else if err := cs.UpsertConcurrency(ctx, testSetID, conc.groups, conc.shared); err != nil {
			logger.Error("failed to save which tests were open together while each call was recorded",
				zap.String("testSetID", testSetID),
				zap.Error(err),
				zap.String("next_step", "keploy record's full round cannot tell which of them made each such call without mappings.yaml's groups; fix the error above and re-record this test set"))
			utils.SetExitCodeOnce(utils.ExitKeployError)
			return
		}
	}
	// Marked after its worker scope is written: a mark that fails leaves the
	// set served by today's rules, under which a routed write must still be
	// answered exactly, never looser.
	if s12 {
		if err := marker.MarkRecordedWith(ctx, testSetID, models.RecordedWithS12Routing); err != nil {
			logger.Error("failed to mark the test set as recorded under S12's routing and serving (mappings.yaml recordedWith): its writes routed to the worker scope are served by the rules of a set recorded before S12, exactly, and required by the set's end",
				zap.String("testSetID", testSetID),
				zap.Error(err),
				zap.String("next_step", "fix the error above and re-record this test set"))
			utils.SetExitCodeOnce(utils.ExitKeployError)
			return
		}
	}
	logger.Info("Saved the test-mock mappings derived from the test windows",
		zap.String("testSetID", testSetID),
		zap.Int("numTests", len(byTest)),
		zap.Int("mappedMocks", mappedMocks),
		zap.Int("workerMocks", len(worker)),
		zap.Int("concurrencyGroups", len(conc.groups)),
		zap.Int("sharedMocks", len(conc.shared)))
	if n := len(concurrent); n > 0 {
		logger.Warn("some tests were recorded while another test's request was still open: a call recorded while several tests were open is listed under the test whose request came first, which time alone cannot tell made it",
			zap.String("testSetID", testSetID),
			zap.Int("count", n),
			zap.Strings("tests", concurrent[:min(n, 20)]),
			zap.String("next_step", "only plain keploy record's full round (KEPLOY_PROVEN=1) gives such a call to the test that made it, when it can tell; every other replay, keploy record sandbox's too, serves it to the test it is listed under. A test whose call keploy cannot settle fails with the reason: record those tests one request at a time"))
	}
}

// concurrencyStore is the part of a mapping store that writes a set's
// concurrency groups and shared calls (models.Mapping.Groups, Shared), whole,
// keeping its other sections.
type concurrencyStore interface {
	UpsertConcurrency(ctx context.Context, testSetID string, groups []models.ConcurrencyGroup, shared []models.MockEntry) error
}

// keptConcurrency is what the recording writes of derived's concurrency
// groups, kept is the test cases it keeps.
type keptConcurrency struct {
	// groups are the groups an entry it writes names, each with the kept
	// tests of it in window order and how many it did not keep (deleted,
	// or lost), in derived's order.
	groups []models.ConcurrencyGroup
	// shared are the grouped mocks whose first-opened test was not kept,
	// when their group has a kept test, in recorded order: any kept test of
	// the group may have made them.
	shared []models.MockEntry
}

// keptConcurrency is derived's concurrency as the recording writes it: an entry
// listed by a kept test keeps its group; one whose test the recording did not
// keep (deleted, or lost) is shared when its group has a kept test, and
// dropped as before when it has none (no kept test was open as it was
// recorded).
func keptConcurrencyOf(derived models.WindowMappings, kept func(id string) bool) keptConcurrency {
	var out keptConcurrency
	if len(derived.Groups) == 0 {
		return out
	}
	byID := make(map[string]models.ConcurrencyGroup, len(derived.Groups))
	for _, g := range derived.Groups {
		k := models.ConcurrencyGroup{ID: g.ID}
		for _, t := range g.Tests {
			if kept(t) {
				k.Tests = append(k.Tests, t)
			} else {
				k.Unkept++
			}
		}
		byID[g.ID] = k
	}
	used := map[string]bool{}
	for _, tc := range derived.Tests {
		for _, e := range tc.Mocks {
			if e.Group == "" {
				continue
			}
			if kept(tc.ID) {
				used[e.Group] = true
				continue
			}
			if len(byID[e.Group].Tests) > 0 {
				used[e.Group] = true
				out.shared = append(out.shared, e)
			}
		}
	}
	for _, g := range derived.Groups {
		if used[g.ID] {
			out.groups = append(out.groups, byID[g.ID])
		}
	}
	sort.SliceStable(out.shared, func(i, j int) bool { return entryAt(out.shared[i]).Before(entryAt(out.shared[j])) })
	return out
}

// entryAt is when an entry's request was recorded.
func entryAt(e models.MockEntry) time.Time {
	at, _ := models.ParseMockTimestamp(e.ReqTimestampMock)
	return at
}

// keptPlaces is derived's worker scope with each place a test case kept
// (kept) holds: a place whose test case is not kept moves back to the latest
// kept test case whose window began before it, in window order, or to the
// set's start before every one.
func keptPlaces(derived models.WindowMappings, kept func(id string) bool) []models.MockEntry {
	if len(derived.Worker) == 0 {
		return nil
	}
	// latestKept is, by test case ID, the latest kept test case at or before
	// it in window order.
	latestKept := make(map[string]string, len(derived.Tests))
	last := ""
	for _, tc := range derived.Tests {
		if kept(tc.ID) {
			last = tc.ID
		}
		latestKept[tc.ID] = last
	}
	out := make([]models.MockEntry, 0, len(derived.Worker))
	for _, e := range derived.Worker {
		if e.Place != "" {
			e.Place = latestKept[e.Place]
		}
		out = append(out, e)
	}
	return out
}

// recordedWithMarker is a mapping store that marks what keploy record
// recorded a set with (models.Mapping.RecordedWith): the YAML store's
// MappingDb.MarkRecordedWith.
type recordedWithMarker interface {
	MarkRecordedWith(ctx context.Context, testSetID, feature string) error
}

// s12Record reports whether this recording is an S12 record, routing its
// in-flight writes and marking its set models.RecordedWithS12Routing, and the
// store that marks it: under the proven switch, while this build's judging
// enables S12's serving (replay.S12Serving: keploy enterprise registers it),
// into a store that keeps the mark. Any other recording routes and marks
// nothing, so its set is served by today's rules, as every set recorded before
// S12 is: keploy alone records nothing S12's serving would serve.
func s12Record(db MappingDb) (recordedWithMarker, bool) {
	if !provenswitch.On() || !replay.S12Serving() {
		return nil, false
	}
	marker, ok := db.(recordedWithMarker)
	return marker, ok
}

// routedReasons writes, on each entry of worker the derivation routed there
// (routed: in flight into a test, sent before it began, on its window's
// clock), why, and its undo (MockEntry.Reason, S12 v16 §4.4), and logs each
// routing once. A test the recording did not keep (kept) is named as one.
func routedReasons(logger *zap.Logger, testSetID string, worker []models.MockEntry, routed []models.RoutedWrite, kept func(id string) bool) {
	if len(routed) == 0 {
		return
	}
	at := make(map[string]int, len(worker))
	for i, e := range worker {
		at[e.Name] = i
	}
	for _, r := range routed {
		i, ok := at[r.Mock.Name]
		if !ok {
			continue
		}
		test, isKept := r.Test, kept(r.Test)
		if strings.HasPrefix(test, lostTestPrefix) {
			test = "a test the recording failed to persist"
		}
		worker[i].Reason = routedReason(r.Mock, test, isKept)
		logger.Info("routed a write to the test set's worker scope: it was sent before the test it was in flight into began, so that test did not make it ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.String("mock", r.Mock.Name),
			zap.String("write", r.Mock.WriteKind),
			zap.String("test", test),
			zap.String("process", processOf(r.Mock, true)))
	}
}

// routedReason is the line a routed write's entry carries (S12 v16 §4.4): what
// it is, the process that sent it, why it is no test's, and its undo. A test
// the recording did not keep (kept false) owns nothing in mappings.yaml, so
// the undo is to delete the entry: the call is then no test's, as that test's
// own calls are.
func routedReason(m models.WindowedMock, test string, kept bool) string {
	why := fmt.Sprintf("%s (%s) routed to background at record: %s sent it before %s began, and it was answered once %s had begun, both stamped on %s, so %s did not make it.",
		m.Name, m.WriteKind, processOf(m, false), test, test, clockName(m.Clock), test)
	if !kept {
		return why + fmt.Sprintf(" Undo: delete this entry: %s is not in this set (the recording did not keep it), so without the routing the call is no test's, as its own calls are", test)
	}
	return why + fmt.Sprintf(" Undo: move this entry under tests[%s].mocks and delete this reason: %s then owns it, as it would without the routing", test, test)
}

// processOf names the process that made m's call: "`sqs` (`./sqs`)"; one
// keploy saw but would not name, why (it runs in a PID namespace of its own)
// and the remedy, with its kernel pid in a log line (log) and without it in
// the committed reason (models.Origin.FileProcessText); else "a process
// keploy could not name".
func processOf(m models.WindowedMock, log bool) string {
	p := m.FileProcess()
	if log {
		p = m.Process()
	}
	if p != "" {
		return p
	}
	return "a process keploy could not name"
}

// clockName says which clock a clock id (fakeconn.ClockID) names.
func clockName(id string) string {
	switch {
	case strings.HasPrefix(id, "proxy:"):
		return "keploy's proxy clock (" + id + ")"
	case strings.HasPrefix(id, "ktime:"):
		return "the kernel's monotonic clock (" + id + ")"
	}
	return "one clock (" + id + ")"
}
