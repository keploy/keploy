package proxy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// s12Set is a set staged as keploy test stages one for exact serving: the
// open test's own (own), another test's (other), a worker-scope recording
// placed at the set's start (w-boot), one placed after T1 (w-later) and a pool
// recording (pool), all in the reusable trees, every one an HTTP call of its
// own path.
func s12Set(t *testing.T) (*MockManager, map[string]*models.Mock) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	ms := map[string]*models.Mock{
		"own":     httpCall("own", 0, "/own"),
		"other":   httpCall("other", time.Second, "/other"),
		"w-boot":  httpCall("w-boot", 2*time.Second, "/w-boot"),
		"w-later": httpCall("w-later", 3*time.Second, "/w-later"),
		"pool":    httpCall("pool", 4*time.Second, "/pool"),
	}
	mm := stagedManager(t, nil, []*models.Mock{ms["own"], ms["other"], ms["w-boot"], ms["w-later"], ms["pool"]})
	return mm, ms
}

// sessionNames is what a reusable read shows, in its order.
func sessionNames(mm *MockManager) string {
	sess, _ := mm.GetSessionScopedMocks()
	return strings.Join(exactNames(sess), ",")
}

// A set served by today's rules shows no background apart, and no pool in a
// test: the background accessors answer nil.
func TestTodaysRulesShowNoBackground(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground("", []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if got := sessionNames(mm); got != "own,w-boot" {
		t.Fatalf("in a test today's rules show %s; want own,w-boot (no pool)", got)
	}
	if b := mm.Background(models.HTTP); b != nil {
		t.Fatalf("today's rules show a background apart: %v", exactNames(b))
	}
	if s := mm.SpentBackground(models.HTTP); s != nil {
		t.Fatalf("today's rules show a spent background: %v", exactNames(s))
	}
	// Nor does a set staged under S12's rules without the proven switch.
	t.Setenv(provenswitch.Env, "")
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if b := mm.Background(models.HTTP); b != nil {
		t.Fatalf("S12's rules apply without the switch: %v", exactNames(b))
	}
}

// Under S12's rules the pool answers in a test too, after the test's own and
// the worker scope it reached; a pool recording a test consumes through a
// delete door is spent for the staging, as the worker scope's is, and listed
// spent (SpentBackground) until the next staging.
func TestS12ShowsThePoolInATestAfterOwnAndWorker(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if got := sessionNames(mm); got != "own,w-boot,pool" {
		t.Fatalf("in a test S12's rules show %s; want own,w-boot,pool", got)
	}
	equalNames(t, "the background", mm.Background(models.HTTP), "w-boot", "pool")
	if got := pollFirst(t, mm, "pool"); got != "pool" {
		t.Fatalf("the pool did not answer in a test: %s", got)
	}
	if got := pollFirst(t, mm, "pool"); got != "a miss" {
		t.Fatalf("a pool recording answered twice in a test: %s", got)
	}
	equalNames(t, "the background after the pool answered", mm.Background(models.HTTP), "w-boot")
	equalNames(t, "the spent background", mm.SpentBackground(models.HTTP), "pool")
	// The gap after: spent for the staging, not the view.
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if got := pollFirst(t, mm, "pool"); got != "a miss" {
		t.Fatalf("a pool recording a test spent answered again in the gap after it: %s", got)
	}
	// Another test's recording is never background.
	for _, mk := range mm.Background(models.HTTP) {
		if mk.Name == "other" || mk.Name == "own" {
			t.Fatalf("the background holds a test's recording: %s", mk.Name)
		}
	}
	// The next staging answers afresh.
	mm.SetMocksWithWindow(nil, []*models.Mock{ms["own"], ms["other"], ms["w-boot"], ms["w-later"], ms["pool"]}, models.BaseTime, time.Now())
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if s := mm.SpentBackground(models.HTTP); len(s) != 0 {
		t.Fatalf("the next staging starts with a spent background: %v", exactNames(s))
	}
}

// Under S12's rules the worker scope is shown in the order the agent placed
// it (ServeExactWorker's), not the trees' (recorded) order; today's rules keep
// the trees'.
func TestS12OrdersTheWorkerScopeAsTheAgentPlacedIt(t *testing.T) {
	mm, _ := s12Set(t)
	mm.ServeExactWorker([]string{"w-later", "w-boot"})
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if got := sessionNames(mm); got != "w-boot,w-later,pool" {
		t.Fatalf("today's rules show %s; want the trees' order w-boot,w-later,pool", got)
	}
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactWorker([]string{"w-later", "w-boot"})
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if got := sessionNames(mm); got != "w-later,w-boot,pool" {
		t.Fatalf("S12's rules show %s; want the agent's order w-later,w-boot, then the pool", got)
	}
	if got := pollFirst(t, mm, "w-"); got != "w-later" {
		t.Fatalf("a parser that takes the first recording took %s; want w-later, placed first", got)
	}
}

// Record's full round pairs only while its session pairs (Learn), under S12's
// rules: without Learn, or under today's rules, there is no candidate and no
// claim.
func TestPairingIsOffWithoutLearn(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if c := mm.PairingCandidates(models.HTTP); c != nil {
		t.Fatalf("a session without Learn has pairing candidates: %v", exactNames(c))
	}
	if mm.ClaimPairing(ms["other"]) {
		t.Fatal("a session without Learn claims a pairing")
	}
	mm.SetPairing(true)
	equalNames(t, "the candidates with Learn", mm.PairingCandidates(models.HTTP), "other")
	// Today's rules pair nothing, Learn or not.
	mm.ServeExactBackground("", nil, nil)
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if c := mm.PairingCandidates(models.HTTP); c != nil {
		t.Fatalf("today's rules have pairing candidates: %v", exactNames(c))
	}
	if mm.ClaimPairing(ms["other"]) {
		t.Fatal("today's rules claim a pairing")
	}
	// Nor without the switch.
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	t.Setenv(provenswitch.Env, "")
	mm.SetPairing(true)
	if c := mm.PairingCandidates(models.HTTP); c != nil {
		t.Fatalf("a session without the switch has pairing candidates: %v", exactNames(c))
	}
}

// A recording is claimed for one pairing in a staging, and never one the open
// test owns, one a call consumed, or one of the background; a claim is no
// consumption, and the next staging claims afresh.
func TestClaimPairingClaimsOnceAndNeverAConsumedOrOwnMock(t *testing.T) {
	mm, ms := s12Set(t)
	extra := httpCall("other-2", 5*time.Second, "/other-2")
	mm.SetMocksWithWindow(nil, []*models.Mock{ms["own"], ms["other"], ms["w-boot"], ms["w-later"], ms["pool"], extra}, models.BaseTime, time.Now())
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.SetPairing(true)
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	equalNames(t, "the candidates", mm.PairingCandidates(models.HTTP), "other", "other-2")
	for _, name := range []string{"own", "w-boot", "w-later", "pool"} {
		if mm.ClaimPairing(ms[name]) {
			t.Fatalf("%s was claimed for a pairing", name)
		}
	}
	if !mm.ClaimPairing(ms["other"]) {
		t.Fatal("another test's recording was not claimed")
	}
	if mm.ClaimPairing(ms["other"]) {
		t.Fatal("a recording was claimed for two pairings")
	}
	equalNames(t, "the candidates after the claim", mm.PairingCandidates(models.HTTP), "other-2")
	// A claim is no consumption: the recording's own test still consumes it.
	mm.ServeExactTest("T2", []*models.Mock{ms["other"]})
	if got := pollFirst(t, mm, "other"); got != "other" {
		t.Fatalf("a claimed recording's test did not consume it: %s", got)
	}
	// Consumed, it is no candidate (its owner consumed it).
	mm.ServeExactTest("T3", nil)
	if mm.ClaimPairing(ms["other"]) {
		t.Fatal("a consumed recording was claimed")
	}
	if err := mm.flagMockAsUsed(models.MockState{Name: "other-2", Kind: models.HTTP, Usage: models.Deleted}, 0); err != nil {
		t.Fatal(err)
	}
	// With T3 open, T1's own (never consumed) is another test's: the one
	// candidate left.
	equalNames(t, "the candidates once other and other-2 are consumed", mm.PairingCandidates(models.HTTP), "own")
	if !mm.ClaimPairing(ms["own"]) || mm.ClaimPairing(ms["own"]) {
		t.Fatal("T1's unconsumed recording was not claimed exactly once")
	}
	// The next staging claims afresh: a claim is the staging's. (What a
	// call consumed stays consumed until the set's boundary, ArmSetFence.)
	mm.SetMocksWithWindow(nil, []*models.Mock{ms["own"], ms["other"], ms["w-boot"], ms["w-later"], ms["pool"], extra}, models.BaseTime, time.Now())
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactTest("T3", nil)
	if !mm.ClaimPairing(ms["own"]) {
		t.Fatal("the next staging did not claim afresh")
	}
	if mm.ClaimPairing(ms["other"]) {
		t.Fatal("a recording its owner consumed in the set was claimed in its next staging")
	}
}

// The pairing reads the recordings the agent keeps on disk too, each once,
// decoding only those of the kind, and ranks them with the trees' in recorded
// order.
func TestPairingCandidatesReadTheDisk(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.SetPairing(true)
	disk, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	onDisk := exactMock("on-disk", 500*time.Millisecond, models.LifetimePerTest)
	onDisk.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: "/disk"}
	mongo := exactMock("mongo-on-disk", 600*time.Millisecond, models.LifetimePerTest)
	mongo.Kind = models.Mongo
	for _, mk := range []*models.Mock{onDisk, mongo, ms["other"]} {
		if err := disk.Add(mk); err != nil {
			t.Fatal(err)
		}
	}
	disk.Finalize()
	mm.SetRecordedOfKind(disk.RangeKind)
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	equalNames(t, "the candidates", mm.PairingCandidates(models.HTTP), "on-disk", "other")
	equalNames(t, "the Mongo candidates", mm.PairingCandidates(models.Mongo), "mongo-on-disk")
	// One its owner consumed is not decoded at all: its name rules it out.
	loaded := map[string]int{}
	mm.SetRecordedOfKind(func(kind models.Kind, visit func(string, func() (*models.Mock, error)) bool) error {
		return disk.RangeKind(kind, func(name string, load func() (*models.Mock, error)) bool {
			return visit(name, func() (*models.Mock, error) { loaded[name]++; return load() })
		})
	})
	if err := mm.flagMockAsUsed(models.MockState{Name: "on-disk", Kind: models.HTTP, Usage: models.Deleted}, 0); err != nil {
		t.Fatal(err)
	}
	equalNames(t, "the candidates once on-disk is consumed", mm.PairingCandidates(models.HTTP), "other")
	if loaded["on-disk"] != 0 {
		t.Fatalf("a consumed on-disk recording was decoded %d times", loaded["on-disk"])
	}
	// A source that fails gives the trees' alone.
	mm.SetRecordedOfKind(func(models.Kind, func(string, func() (*models.Mock, error)) bool) error {
		return errors.New("the store is closed")
	})
	equalNames(t, "the candidates with a failed source", mm.PairingCandidates(models.HTTP), "other")
}

// RangeKind visits the store's mocks of one kind, in recorded order, until
// visit says stop, decoding only what visit loads.
func TestDiskMocksRangeKind(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	disk, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	late := exactMock("late", 2*time.Second, models.LifetimePerTest)
	early := exactMock("early", time.Second, models.LifetimePerTest)
	mongo := exactMock("mongo", 0, models.LifetimePerTest)
	mongo.Kind = models.Mongo
	for _, mk := range []*models.Mock{late, early, mongo} {
		if err := disk.Add(mk); err != nil {
			t.Fatal(err)
		}
	}
	disk.Finalize()
	var names []string
	loaded := 0
	if err := disk.RangeKind(models.HTTP, func(name string, load func() (*models.Mock, error)) bool {
		names = append(names, name)
		if name == "early" {
			mk, err := load()
			if err != nil || mk.Name != "early" {
				t.Fatalf("load = %v, %v", mk, err)
			}
			loaded++
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "early,late" || loaded != 1 {
		t.Fatalf("RangeKind visited %v (loaded %d); want early,late in recorded order, loading only early", names, loaded)
	}
	names = nil
	_ = disk.RangeKind(models.HTTP, func(name string, _ func() (*models.Mock, error)) bool {
		names = append(names, name)
		return false
	})
	if strings.Join(names, ",") != "early" {
		t.Fatalf("RangeKind went on past a stop: %v", names)
	}
	// A mock added again is visited once; one added again as another kind
	// is that kind's alone.
	again, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	twice := exactMock("twice", time.Second, models.LifetimePerTest)
	moved := exactMock("moved", 2*time.Second, models.LifetimePerTest)
	movedMongo := exactMock("moved", 2*time.Second, models.LifetimePerTest)
	movedMongo.Kind = models.Mongo
	for _, mk := range []*models.Mock{twice, twice, moved, movedMongo} {
		if err := again.Add(mk); err != nil {
			t.Fatal(err)
		}
	}
	again.Finalize()
	visit := func(kind models.Kind) string {
		var got []string
		_ = again.RangeKind(kind, func(name string, _ func() (*models.Mock, error)) bool {
			got = append(got, name)
			return true
		})
		return strings.Join(got, ",")
	}
	if got := visit(models.HTTP); got != "twice" {
		t.Fatalf("HTTP's mocks are %q; want twice once, and not moved (now Mongo's)", got)
	}
	if got := visit(models.Mongo); got != "moved" {
		t.Fatalf("Mongo's mocks are %q; want moved", got)
	}
}

// Each replay session tells the manager whether it pairs: record's full
// round's (OutgoingOptions.Learn), under the switch; a session after it that
// does not learn pairs nothing.
func TestAReplaySessionPairsOnlyWithLearn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := New(zap.NewNop(), nil, &config.Config{})
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()
	p.setMockManager(mm)
	_ = p.Mock(context.Background(), models.OutgoingOptions{Learn: true})
	if !mm.exact.pairs.Load() {
		t.Fatal("record's full round's session does not pair")
	}
	_ = p.Mock(context.Background(), models.OutgoingOptions{})
	if mm.exact.pairs.Load() {
		t.Fatal("a session without Learn pairs")
	}
}

// A replay session forgets the previous set's background rules: until the
// next staging names its own, today's serve.
func TestASessionResetForgetsTheBackgroundRules(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if b := mm.Background(models.HTTP); len(b) == 0 {
		t.Fatal("precondition: S12's rules show no background")
	}
	mm.ResetForReplaySession()
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if b := mm.Background(models.HTTP); b != nil {
		t.Fatalf("after a session reset the background is still served by S12's rules: %v", exactNames(b))
	}
}

// A pairing's report (a provisional answer) lost for room makes the read
// incomplete, as a miss's does (record's judging reads it), and the cause
// says it was no miss. A by-kind answer's report lost for room, in a test's
// window or waiting for the next, makes no read incomplete (no judging reads
// it): it is counted, and said.
func TestALostAnswerReportIsSaidToBeNoMiss(t *testing.T) {
	answer := models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: models.ErrAnswered,
		MismatchReport: &models.MockMismatchReport{Protocol: "Mongo", Answer: models.AnswerProvisional, ClosestMock: "mock-6", RecordedIn: "post-url-1"}}
	p := &Proxy{}
	p.noteMissLost(answer, missLostListFull)
	cause := p.missLoss.Load()
	if cause == nil || !strings.HasPrefix(*cause, "an answered call's report (no miss) was lost: ") || !strings.Contains(*cause, missLostListFull) {
		t.Fatalf("the loss of a pairing's report is %v; want it said as no miss, with the list's cause", cause)
	}
	p = &Proxy{}
	p.pendingDroppedForRoom(answer)
	if !p.pendingLost {
		t.Fatal("a pairing's report dropped while no window was open left the next read complete")
	}

	byKind := models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: models.ErrAnswered,
		MismatchReport: &models.MockMismatchReport{Protocol: "Mongo", Answer: models.AnswerByKind, WriteKind: "Mongo insert keploy.events"}}
	core, logs := observer.New(zapcore.WarnLevel)
	p = &Proxy{logger: zap.New(core)}
	p.noteMissLost(byKind, missLostListFull)
	p.pendingDroppedForRoom(byKind)
	if cause := p.missLoss.Load(); cause != nil || p.pendingLost {
		t.Fatalf("a by-kind answer's lost report made a read incomplete: %v, pending %v", cause, p.pendingLost)
	}
	if got := p.answersDropped.Load(); got != 2 {
		t.Fatalf("%d by-kind answers counted dropped; want 2", got)
	}
	if n := logs.FilterMessageSnippet("a background write answered by its kind was not kept").Len(); n != 1 {
		t.Fatalf("the dropped by-kind answers were said %d times; want once, for the first", n)
	}
	miss := models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: errors.New("no match"),
		MismatchReport: &models.MockMismatchReport{Protocol: "Mongo"}}
	p = &Proxy{}
	p.noteMissLost(miss, missLostListFull)
	if cause := p.missLoss.Load(); cause == nil || *cause != missLostListFull {
		t.Fatalf("a miss's loss is %v; want %q", cause, missLostListFull)
	}
}

// An async lane's pool recording keeps today's rules under S12's: it is not
// shown in a test, nor spent there, nor background (S12 v16 §2.1).
func TestS12LeavesAnAsyncPoolRecordingOutOfATest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own := httpCall("own", 0, "/own")
	lane := httpCall("lane", time.Second, "/lane")
	lane.Spec.Async = &models.AsyncMeta{}
	pool := httpCall("pool", 2*time.Second, "/pool")
	mm := stagedManager(t, nil, []*models.Mock{own, lane, pool})
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"lane", "pool"}, nil)
	mm.ServeExactWorker(nil)
	mm.ServeExactTest("T1", []*models.Mock{own})
	if got := sessionNames(mm); got != "own,pool" {
		t.Fatalf("in a test S12's rules show %s; want own,pool (no async lane's)", got)
	}
	equalNames(t, "the background", mm.Background(models.HTTP), "pool")
	if v := mm.exactView(); v.inBackgroundPool(lane) {
		t.Fatal("an async lane's pool recording is spent as background in a test")
	}
}

// Under S12's rules the pool is shown in recorded order whichever trees its
// recordings were staged in, as the background accessor gives it: the
// earlier-recorded one first whether it or the later one is staged in the
// per-test tier (one of the two stagings puts them in the trees against
// their recorded order).
func TestS12ShowsThePoolInRecordedOrder(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, perTestOne := range []string{"pool-later", "pool-earlier"} {
		t.Run(perTestOne+" per-test", func(t *testing.T) {
			own := httpCall("own", 0, "/own")
			later := httpCall("pool-later", 3*time.Second, "/pool")
			earlier := httpCall("pool-earlier", time.Second, "/pool")
			perTest, session := later, earlier
			if perTestOne == "pool-earlier" {
				perTest, session = earlier, later
			}
			perTest.TestModeInfo.Lifetime = models.LifetimePerTest
			mm := stagedManager(t, []*models.Mock{perTest}, []*models.Mock{own, session})
			mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool-later", "pool-earlier"}, nil)
			mm.ServeExactWorker(nil)
			mm.ServeExactTest("T1", []*models.Mock{own})
			equalNames(t, "the background", mm.Background(models.HTTP), "pool-earlier", "pool-later")
		})
	}
}

// Under S12's rules the background's stateful cursor is the one the calls
// between tests advance, inside a test too (S12 v16 §2.3: the pool keeps its
// recorded reuse and cursor-saturate, its cursor shared with background
// callers): a mark a call between tests made is read in a test, one a call in
// a test made is read between tests, while the store's own cursor keeps none
// in a test, as before. Under today's rules there is no background cursor;
// through a scoped view it is in the view's scope; a staging resets it.
func TestS12SharesTheBackgroundsCursorBetweenATestAndTheGaps(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground("", []string{"pool"}, []string{"w-boot"})
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if _, ok := mm.BackgroundCursor(); ok {
		t.Fatal("today's rules have a background cursor")
	}

	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot"})
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	bg, ok := mm.BackgroundCursor()
	if !ok {
		t.Fatal("S12's rules have no background cursor")
	}
	// Between tests: the store's cursor and the background's are one.
	mm.AdvanceMockCursor("served\x00pool", 0, 2)
	if got := bg.MockCursorIndex("served\x00pool", 2); got != 1 {
		t.Fatalf("between tests the background's cursor reads %d after the store's advanced; want 1", got)
	}
	// In a test: the store's keeps none; the background's is the gaps'.
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if got := mm.MockCursorIndex("served\x00pool", 2); got != 0 {
		t.Fatalf("in a test the store's cursor reads %d; want 0 (none kept)", got)
	}
	if got := bg.MockCursorIndex("served\x00pool", 2); got != 1 {
		t.Fatalf("in a test the background's cursor reads %d; want the gap's 1", got)
	}
	bg.AdvanceMockCursor("served\x00other", 0, 2)
	mm.AdvanceMockCursor("served\x00own", 0, 2) // none kept in a test
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if got := mm.MockCursorIndex("served\x00other", 2); got != 1 {
		t.Fatalf("between tests the store's cursor reads %d for a mark made in a test through the background's; want 1", got)
	}
	if got := mm.MockCursorIndex("served\x00own", 2); got != 0 {
		t.Fatalf("a test's own mark was kept: %d", got)
	}

	// A scoped view forwards it, its keys those of the view's other cursor
	// keys (under exact serving the bare ones: the view passes through).
	var sv integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	sbg, ok := integrations.BackgroundCursorOf(sv)
	if !ok {
		t.Fatal("a scoped view drops the background cursor")
	}
	sv.(integrations.MockCursor).AdvanceMockCursor("served\x00scoped", 0, 2)
	if got := sbg.MockCursorIndex("served\x00scoped", 2); got != 1 {
		t.Fatalf("the scoped background cursor reads %d after the scoped view's advanced; want 1", got)
	}
	if got := bg.MockCursorIndex("served\x00scoped", 2); got != 1 {
		t.Fatalf("the scoped view's mark is not the bare store's: %d", got)
	}

	// A session reset forgets the rules: no background cursor.
	mm.ResetForReplaySession()
	if _, ok := mm.BackgroundCursor(); ok {
		t.Fatal("a reset session kept a background cursor")
	}
}

// A window's answered calls' reports have room of their own: a background
// writer answered by its kind more often than a window keeps reports of
// leaves every miss of the window kept, and a miss's room filled leaves an
// answer's report kept (S12 v16 §3: by-kind reuse is unbounded). A drain
// empties both.
func TestAnswersAndMissesHaveRoomOfTheirOwn(t *testing.T) {
	answer := models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: models.ErrAnswered,
		MismatchReport: &models.MockMismatchReport{Protocol: "Mongo", Answer: models.AnswerByKind, WriteKind: "Mongo insert keploy.events"}}
	miss := models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: errors.New("no match"),
		MismatchReport: &models.MockMismatchReport{Protocol: "Mongo"}}
	acc := &testErrorAccumulator{}
	for i := 0; i < maxPendingMockErrors; i++ {
		if !acc.addBounded(answer, maxPendingMockErrors) {
			t.Fatalf("answer %d was dropped before its room was full", i+1)
		}
	}
	if acc.addBounded(answer, maxPendingMockErrors) {
		t.Fatal("an answer past its room was kept")
	}
	if !acc.addBounded(miss, maxPendingMockErrors) {
		t.Fatal("a miss was dropped because the window's answers were full")
	}
	// An answer made before the set's first test is carried wrapped
	// (beforeSetMiss): still an answer, in the answers' room.
	if acc.addBounded(beforeSetMiss{answer}, maxPendingMockErrors) {
		t.Fatal("an answer carried as the set's was kept past the answers' room")
	}
	for i := 1; i < maxPendingMockErrors; i++ {
		if !acc.addBounded(miss, maxPendingMockErrors) {
			t.Fatalf("miss %d was dropped before its room was full", i+1)
		}
	}
	if acc.addBounded(miss, maxPendingMockErrors) {
		t.Fatal("a miss past its room was kept")
	}
	if got := len(acc.drain()); got != 2*maxPendingMockErrors {
		t.Fatalf("the window held %d reports; want %d answers and %d misses", got, maxPendingMockErrors, maxPendingMockErrors)
	}
	if !acc.addBounded(answer, maxPendingMockErrors) || acc.answers != 1 {
		t.Fatalf("after a drain an answer's room is not empty: %d", acc.answers)
	}
}

// Answering by kind reads a kind's whole background, whatever places the run
// has reached (S12 v16 §2.3: by-kind answers ignore Place; §3: a kind is
// answered by kind only when every background reply of it is equal):
// KindBackground lists a worker recording placed after a test not begun,
// which Background, what exact serving reads, hides; and SpendByKind spends it
// for the staging, once, so it answers nothing more once its place is
// reached. A test's own or another test's recording is no background; today's
// rules have none; a scoped view forwards it.
func TestKindBackgroundIgnoresPlace(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	equalNames(t, "the background exact serving reads", mm.Background(models.HTTP), "w-boot", "pool")
	unused, spent := mm.KindBackground(models.HTTP)
	equalNames(t, "the kind's unused background", unused, "w-boot", "w-later", "pool")
	equalNames(t, "the kind's spent background", spent)

	for _, name := range []string{"own", "other"} {
		if mm.SpendByKind(ms[name]) {
			t.Fatalf("a by-kind answer spent %s, a test's recording", name)
		}
	}
	if !mm.SpendByKind(ms["w-later"]) {
		t.Fatal("a by-kind answer could not spend a recording placed after a test not begun")
	}
	if mm.SpendByKind(ms["w-later"]) {
		t.Fatal("a by-kind answer spent a recording twice")
	}
	unused, spent = mm.KindBackground(models.HTTP)
	equalNames(t, "the kind's unused background after the spend", unused, "w-boot", "pool")
	equalNames(t, "the kind's spent background after the spend", spent, "w-later")
	equalNames(t, "the spent background", mm.SpentBackground(models.HTTP), "w-later")

	// Its place reached, it is spent for the staging: not shown.
	mm.ServeExactWorker([]string{"w-boot", "w-later"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	equalNames(t, "the background once w-later's place is reached", mm.Background(models.HTTP), "w-boot", "pool")
	if got := pollFirst(t, mm, "w-later"); got != "a miss" {
		t.Fatalf("a recording a by-kind answer spent answered again: %s", got)
	}

	// Through a scoped view.
	var sv integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	su, ss := integrations.KindBackground(sv, models.HTTP)
	equalNames(t, "the kind's unused background through a scoped view", su, "w-boot", "pool")
	equalNames(t, "the kind's spent background through a scoped view", ss, "w-later")
	if !integrations.SpendByKind(sv, ms["pool"]) {
		t.Fatal("a scoped view could not spend by kind")
	}

	// Today's rules: none.
	mm.ServeExactBackground("", []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if u, s := mm.KindBackground(models.HTTP); u != nil || s != nil {
		t.Fatalf("today's rules have a by-kind background: %v, %v", exactNames(u), exactNames(s))
	}
	if mm.SpendByKind(ms["w-boot"]) {
		t.Fatal("today's rules spent by kind")
	}
}

// An async lane's worker-scope recording keeps today's reuse under S12's
// rules (S12 v16 §2.1): the view shows it, as today, but it is no
// background: exact serving's background (Background, SpentBackground) and
// the kind's whole background (KindBackground) leave it out, and no by-kind
// answer spends it, whether its place is reached or not.
func TestS12LeavesAnAsyncWorkerRecordingOutOfTheBackground(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own := httpCall("own", 0, "/own")
	lane := httpCall("w-lane", time.Second, "/lane")
	lane.Spec.Async = &models.AsyncMeta{}
	laneLater := httpCall("w-lane-later", 2*time.Second, "/lane")
	laneLater.Spec.Async = &models.AsyncMeta{}
	boot := httpCall("w-boot", 3*time.Second, "/w-boot")
	mm := stagedManager(t, nil, []*models.Mock{own, lane, laneLater, boot})
	mm.ServeExactBackground(models.RecordedWithS12Routing, nil, []string{"w-lane", "w-lane-later", "w-boot"})
	mm.ServeExactWorker([]string{"w-lane", "w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{own})
	if got := sessionNames(mm); !strings.Contains(got, "w-lane") {
		t.Fatalf("the view hides the async lane's worker recording (%s): today's reuse shows it", got)
	}
	equalNames(t, "the background", mm.Background(models.HTTP), "w-boot")
	unused, spent := mm.KindBackground(models.HTTP)
	equalNames(t, "the kind's unused background", unused, "w-boot")
	equalNames(t, "the kind's spent background", spent)
	for _, mk := range []*models.Mock{lane, laneLater} {
		if mm.SpendByKind(mk) {
			t.Fatalf("a by-kind answer spent %s, an async lane's", mk.Name)
		}
	}
	equalNames(t, "the spent background", mm.SpentBackground(models.HTTP))
}

// SpentBackground lists the staging's spent background in recorded order,
// whatever order the calls spent it in: what a by-kind reuse answers with
// (S12 v16 §3).
func TestSpentBackgroundIsInRecordedOrder(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	// The per-test tier, which the trees read first, holds the
	// later-recorded pool recording.
	pool := httpCall("pool", 4*time.Second, "/pool")
	pool.TestModeInfo.Lifetime = models.LifetimePerTest
	boot := httpCall("w-boot", time.Second, "/w-boot")
	later := httpCall("w-later", 2*time.Second, "/w-later")
	mm := stagedManager(t, []*models.Mock{pool}, []*models.Mock{later, boot})
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	for _, mk := range []*models.Mock{pool, later, boot} {
		if !mm.SpendByKind(mk) {
			t.Fatalf("could not spend %s", mk.Name)
		}
	}
	equalNames(t, "the spent background", mm.SpentBackground(models.HTTP), "w-boot", "w-later", "pool")
	_, spent := mm.KindBackground(models.HTTP)
	equalNames(t, "the kind's spent background", spent, "w-boot", "w-later", "pool")
}
