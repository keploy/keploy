package consumer_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer/consumerfake"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// rig is a recording: a real SyncMockManager with its real dedup queue, a real
// recorder, and the channels a recording drains.
//
// It deliberately uses the PRODUCTION window authority rather than a stand-in.
// The whole risk in the record path is attribution — which mocks belong to
// which test — and a fake queue would test the recorder's arithmetic while
// leaving the thing that actually decides untested.
type rig struct {
	t        *testing.T
	mgr      *syncMock.SyncMockManager
	rec      *consumer.Recorder
	ctx      context.Context
	mocks    chan *models.Mock
	mappings chan models.TestMockMapping
	tests    chan *models.TestCase
	clk      *consumerfake.Clock
	now      time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	t.Cleanup(consumerfake.Register())

	mgr := syncMock.New(zap.NewNop())
	mocks := make(chan *models.Mock, 256)
	mappings := make(chan models.TestMockMapping, 64)
	tests := make(chan *models.TestCase, 64)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mgr.SetOutputChannel(mocks)
	mgr.SetMappingChannel(ctx, mappings)

	clk := consumerfake.NewClock(time.Time{})
	rec := consumer.NewRecorder(consumer.RecorderOptions{
		Logger:        zap.NewNop(),
		Clock:         clk,
		TestCases:     tests,
		EnableMapping: true,
	})
	// THE PRODUCTION SIDE-EFFECT INGEST. Proxy.installConsumerEgressObserver
	// does exactly this for a real recording session: the syncMock manager
	// runs the recorder over every mock it keeps, whichever parser emitted it.
	// The rig registers it too, because the alternative — counting untagged
	// mocks from the consumer parser's own OnMock call — is a thing no real
	// deployment can do: a Kafka worker's database INSERT is emitted by the
	// postgres parser, which has never heard of this contract.
	mgr.SetEgressObserver(rec.OnEgress)
	return &rig{
		t:        t,
		mgr:      mgr,
		rec:      rec,
		ctx:      syncMock.NewContext(ctx, mgr),
		mocks:    mocks,
		mappings: mappings,
		tests:    tests,
		clk:      clk,
		now:      clk.Now(),
	}
}

// at advances the recording clock and returns the instant. The recorder's own
// clock moves with it, so the window the last unit is closed at is the one the
// test laid out rather than whatever the clock happened to read.
func (r *rig) at(d time.Duration) time.Time {
	r.now = r.now.Add(d)
	r.clk.Set(r.now)
	return r.now
}

// emit is what a CONSUMER-AWARE parser does with a mock: hand it to the
// consumer recorder first, then to the syncMock buffer.
func (r *rig) emit(m *models.Mock) {
	r.t.Helper()
	r.rec.OnMock(r.ctx, m)
	r.mgr.AddMock(m)
}

// emitFromAnotherParser is what EVERY OTHER parser in the tree does: hand the
// mock straight to the syncMock manager, with no idea the consumer contract
// exists. This is how a Kafka worker's database INSERT actually arrives.
func (r *rig) emitFromAnotherParser(m *models.Mock) {
	r.t.Helper()
	r.mgr.AddMock(m)
}

func (r *rig) trigger(reqAt, resAt time.Time, connID string, views ...models.EffectView) *models.Mock {
	return consumerfake.Mock(consumerfake.MockOptions{
		Name:   "kafka-fetch",
		Role:   models.RoleTrigger,
		Views:  views,
		ReqAt:  reqAt,
		ResAt:  resAt,
		ConnID: connID,
	})
}

func (r *rig) effect(at time.Time, views ...models.EffectView) *models.Mock {
	return consumerfake.Mock(consumerfake.MockOptions{
		Name:  "kafka-produce",
		Role:  models.RoleEffect,
		Views: views,
		ReqAt: at,
		ResAt: at,
	})
}

// sideEffect stands in for the database write a consume-and-write worker
// makes: a mock of a DIFFERENT protocol family, carrying no role tag at all.
func (r *rig) sideEffect(at time.Time) *models.Mock {
	return consumerfake.Mock(consumerfake.MockOptions{
		Name:  "postgres-insert",
		Kind:  consumerfake.SideEffectKind,
		ReqAt: at,
		ResAt: at,
	})
}

func (r *rig) drainTests() []*models.TestCase {
	var out []*models.TestCase
	for {
		select {
		case tc := <-r.tests:
			out = append(out, tc)
		default:
			return out
		}
	}
}

func (r *rig) drainMappings() map[string][]string {
	out := map[string][]string{}
	for {
		select {
		case m := <-r.mappings:
			out[m.TestName] = append(out[m.TestName], m.MockIDs...)
		default:
			return out
		}
	}
}

func (r *rig) drainMocks() []*models.Mock {
	var out []*models.Mock
	for {
		select {
		case m := <-r.mocks:
			out = append(out, m)
		default:
			return out
		}
	}
}

func produce(key string) models.EffectView {
	return consumerfake.View("produce", "order-events", key, `{"status":"CONFIRMED"}`)
}

func fetch(key string) models.EffectView {
	return consumerfake.View("fetch", "orders", key, `{"orderId":"`+key+`"}`)
}

// A HEADLESS CONSUMER PRODUCES A MAPPING. This is the proof the record half
// works at all: today a worker with no HTTP ingress produces an EMPTY
// mappings.yaml, because the syncMock buffer is never armed — the first-request
// flag has exactly two non-test callers in the tree and both are HTTP ingress —
// so nothing is ever binned into a window.
func TestAHeadlessConsumerProducesMappings(t *testing.T) {
	r := newRig(t)

	t1req, t1res := r.at(0), r.at(10*time.Millisecond)
	r.emit(r.trigger(t1req, t1res, "c1", fetch("o-1")))
	r.emit(r.effect(r.at(30*time.Millisecond), produce("o-1")))

	t2req, t2res := r.at(300*time.Millisecond), r.at(10*time.Millisecond)
	r.emit(r.trigger(t2req, t2res, "c1", fetch("o-2")))
	r.emit(r.effect(r.at(25*time.Millisecond), produce("o-2")))

	r.rec.Close(r.ctx)

	mappings := r.drainMappings()
	if len(mappings) == 0 {
		t.Fatal("a headless consumer recording produced NO mappings; without them replay cannot arm exactly one trigger and the whole feature is inert")
	}
	if got := len(mappings["test-1"]); got != 2 {
		t.Fatalf("test-1 mapped %d mocks, want 2 (its trigger and its effect): %v", got, mappings)
	}
	if got := len(mappings["test-2"]); got != 2 {
		t.Fatalf("test-2 mapped %d mocks, want 2: %v", got, mappings)
	}

	tests := r.drainTests()
	if len(tests) != 2 {
		t.Fatalf("minted %d test cases, want 2", len(tests))
	}
	for _, tc := range tests {
		if tc.Kind != models.CONSUMER {
			t.Fatalf("%s: kind %q", tc.Name, tc.Kind)
		}
		if tc.ConsumerSpec == nil {
			t.Fatalf("%s: no consumer spec", tc.Name)
		}
		if tc.ConsumerSpec.Completion.ExpectEffects != 1 {
			t.Fatalf("%s: expectEffects=%d, want 1", tc.Name, tc.ConsumerSpec.Completion.ExpectEffects)
		}
	}
	if err := r.rec.Stats().Err(); err != nil {
		t.Fatalf("a clean recording must reconcile: %v", err)
	}
}

// THE TRIGGER IS BOUND TO ITS UNIT BY IDENTITY, NOT BY TIMESTAMP.
//
// A unit's window starts at its trigger's RESPONSE time, so the trigger's own
// request time falls outside it and inside the PREVIOUS unit's window. Bound by
// timestamp, every trigger would be attributed to the previous test — and once
// a recording is past the five-test startup window, dropped outright by the
// stale-buffer cutoff, leaving replay with no bytes to deliver.
func TestEachTriggerIsMappedToItsOwnTestNotThePreviousOne(t *testing.T) {
	r := newRig(t)

	// Enough units to run past models.StartupMockTestCaseWindow (5), which
	// is what makes the difference between "rescued but unmapped" and
	// "silently reaped".
	const units = 8
	for i := 0; i < units; i++ {
		req := r.at(300 * time.Millisecond)
		res := r.at(10 * time.Millisecond)
		r.emit(r.trigger(req, res, "c1", fetch("o")))
		r.emit(r.effect(r.at(20*time.Millisecond), produce("o")))
	}
	r.rec.Close(r.ctx)

	mappings := r.drainMappings()
	for i := 1; i <= units; i++ {
		name := "test-" + itoa(i)
		if got := len(mappings[name]); got != 2 {
			t.Errorf("%s mapped %d mocks, want 2 (trigger + effect); a trigger bound by timestamp lands on the previous test or is reaped", name, got)
		}
	}
}

// The in-flight unit tag must never reach disk: mocks.yaml keeps the shape it
// has today, and the mapping stays the single persisted statement of which
// mocks belong to which test.
func TestTheInFlightUnitTagIsStrippedBeforeAMockIsWritten(t *testing.T) {
	r := newRig(t)
	req, res := r.at(0), r.at(10*time.Millisecond)
	r.emit(r.trigger(req, res, "c1", fetch("o-1")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-1")))
	r.rec.Close(r.ctx)

	for _, m := range r.drainMocks() {
		if _, leaked := m.Spec.Metadata[models.MetaKeyUnitTest]; leaked {
			t.Fatalf("mock %q was written carrying the in-flight unit tag %q", m.Name, models.MetaKeyUnitTest)
		}
	}
}

// NO CROSS-ATTRIBUTION WITH AN INTERLEAVED HTTP REQUEST. A consumer worker
// that also serves a health endpoint puts jobs on the SAME dedup queue, which
// drains strictly from its head — which is exactly why the recorder goes
// through the queue instead of calling the window resolver directly: a parser
// resolving ranges on its own produces windows that interleave with the queued
// ingress jobs, and the failure mode is silent.
//
// The health request here lands between two consumer units, after an empty
// poll has closed the first one. That is the real shape: a consumer polls
// continuously, so its empty polls keep each unit's window tight around the
// message it handled instead of spanning the idle stretch. This is the SECOND
// reason close-on-idle-poll is mandatory — the first is the stale-buffer
// horizon — and without it this test's health-check mock lands inside the open
// consumer window and is attributed to the consumer test.
func TestAnInterleavedHTTPRequestDoesNotStealTheConsumersMocks(t *testing.T) {
	r := newRig(t)

	t1req, t1res := r.at(0), r.at(10*time.Millisecond)
	r.emit(r.trigger(t1req, t1res, "c1", fetch("o-1")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-1")))
	// The consumer polls again and the broker has nothing: unit one closes.
	r.emit(r.trigger(r.at(20*time.Millisecond), r.at(5*time.Millisecond), "c1"))

	// An HTTP /health request arrives and is answered, exactly as the
	// ingress path drives it.
	httpStart := r.at(5 * time.Millisecond)
	job := r.mgr.DedupQueue().Enqueue(httpStart)
	httpMock := consumerfake.Mock(consumerfake.MockOptions{
		Name:  "http-health-dep",
		Kind:  consumerfake.SideEffectKind,
		ReqAt: r.at(1 * time.Millisecond),
		ResAt: r.now,
	})
	r.mgr.AddMock(httpMock) // NOT r.emit: this is ingress traffic, not the consumer's
	r.mgr.DedupQueue().ResolveJob(job, false, r.at(1*time.Millisecond), "http-test-1", true, r.mgr)

	t2req, t2res := r.at(300*time.Millisecond), r.at(10*time.Millisecond)
	r.emit(r.trigger(t2req, t2res, "c1", fetch("o-2")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-2")))
	r.rec.Close(r.ctx)

	mappings := r.drainMappings()
	if got := len(mappings["http-test-1"]); got != 1 {
		t.Fatalf("the HTTP request mapped %d mocks, want exactly its own 1: %v", got, mappings)
	}
	// Unit one's own traffic is its trigger, its effect, and the empty poll
	// that closed it — three mocks, and not the health check's.
	if got := len(mappings["test-1"]); got != 3 {
		t.Fatalf("test-1 mapped %d mocks, want its own 3 (trigger, effect, closing empty poll): %v", got, mappings)
	}
	if got := len(mappings["test-2"]); got != 2 {
		t.Fatalf("test-2 mapped %d mocks, want 2: %v", got, mappings)
	}
	// And nothing was mapped twice: six mocks were emitted, six were
	// attributed, each to exactly one test.
	total := 0
	for _, ids := range mappings {
		total += len(ids)
	}
	if total != 6 {
		t.Fatalf("%d mock attributions across %v; six mocks were emitted and each belongs to exactly one test", total, mappings)
	}
}

// A CONSUMER IDLING FOR MORE THAN SEVEN SECONDS LOSES NOTHING.
//
// Opening a unit sets the syncMock first-request flag, which makes every mock
// buffer — and the buffer's safety valve reaps out-of-window per-test mocks
// older than seven seconds. A consumer that idles for more than seven seconds
// between messages is the NORMAL case, so this is the blast radius that has to
// be measured rather than assumed.
func TestSevenSecondGapsBetweenMessagesDropNoMocks(t *testing.T) {
	r := newRig(t)

	const units = 3
	for i := 0; i < units; i++ {
		req := r.at(9 * time.Second) // longer than the 7s stale cutoff
		res := r.at(10 * time.Millisecond)
		r.emit(r.trigger(req, res, "c1", fetch("o")))
		r.emit(r.effect(r.at(40*time.Millisecond), produce("o")))
	}
	r.rec.Close(r.ctx)

	mappings := r.drainMappings()
	for i := 1; i <= units; i++ {
		name := "test-" + itoa(i)
		if got := len(mappings[name]); got != 2 {
			t.Errorf("%s mapped %d mocks, want 2: a >7s idle stretch reaped part of the unit", name, got)
		}
	}
	if got := len(r.drainMocks()); got != units*2 {
		t.Errorf("%d mocks reached persistence, want %d — the stale-buffer cutoff dropped some", got, units*2)
	}
}

// An empty poll response is not a unit; it is what ends one. Closing on it is
// what keeps a window from straddling the stale-buffer horizon during an idle
// stretch.
func TestAnEmptyPollClosesTheOpenUnitWithoutOpeningANewOne(t *testing.T) {
	r := newRig(t)

	req, res := r.at(0), r.at(10*time.Millisecond)
	r.emit(r.trigger(req, res, "c1", fetch("o-1")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-1")))
	// An empty poll: a trigger frame that projects to no records.
	r.emit(r.trigger(r.at(500*time.Millisecond), r.at(10*time.Millisecond), "c1"))

	tests := r.drainTests()
	if len(tests) != 1 {
		t.Fatalf("minted %d test cases, want 1 — an empty poll must close a unit, never open one", len(tests))
	}
	stats := r.rec.Stats()
	if stats.IdlePolls != 1 {
		t.Fatalf("idle polls counted: %d, want 1", stats.IdlePolls)
	}
	if stats.UnitsObserved != 1 {
		t.Fatalf("units observed: %d, want 1", stats.UnitsObserved)
	}
}

// A unit that produced nothing observable can only ever PASS. Recording it
// would manufacture a vacuous green, so it is refused by name at mint.
func TestAUnitWithNoObservableEffectIsRefusedByName(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	r.emit(r.trigger(r.at(300*time.Millisecond), r.at(10*time.Millisecond), "c1", fetch("o-2")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-2")))
	r.rec.Close(r.ctx)

	if got := len(r.drainTests()); got != 1 {
		t.Fatalf("minted %d test cases, want 1 — a unit with no observable effect must not become a file", got)
	}
	stats := r.rec.Stats()
	if stats.UnitsRefused != 1 {
		t.Fatalf("refused %d units, want 1", stats.UnitsRefused)
	}
	if stats.Refusals[0].Category != models.CategoryConsumerNoObservableEffect {
		t.Fatalf("category %q, want %q", stats.Refusals[0].Category, models.CategoryConsumerNoObservableEffect)
	}
	if err := stats.Err(); err == nil {
		t.Fatal("a refused unit must fail the recording out loud")
	}
	// THE WINDOW OF A REFUSED UNIT IS STILL RESOLVED. The dedup queue
	// drains strictly from its head, so a job left unresolved wedges every
	// job behind it for the rest of the recording. A refusal must cost one
	// test case, never the whole recording's mock attribution.
	mappings := r.drainMappings()
	if got := len(mappings["test-2"]); got != 2 {
		t.Fatalf("test-2 mapped %d mocks, want 2: the refused unit left its job unresolved and wedged the queue behind it (%v)", got, mappings)
	}
}

// A unit that was observed, not refused, and never reached persistence has
// VANISHED. That is the one outcome the reconciliation exists to forbid: a
// user who watched ten messages go by must be told when only nine files exist.
func TestAUnitThatNeverReachedPersistenceIsReportedAsLost(t *testing.T) {
	t.Cleanup(consumerfake.Register())
	mgr := syncMock.New(zap.NewNop())
	mocks := make(chan *models.Mock, 64)
	mgr.SetOutputChannel(mocks)

	ctx, cancel := context.WithCancel(context.Background())
	mctx := syncMock.NewContext(ctx, mgr)
	clk := consumerfake.NewClock(time.Time{})
	// An unbuffered channel nobody reads: the hand-off cannot complete.
	rec := consumer.NewRecorder(consumer.RecorderOptions{
		Logger: zap.NewNop(), Clock: clk, TestCases: make(chan *models.TestCase),
	})

	now := clk.Now()
	tr := consumerfake.Mock(consumerfake.MockOptions{
		Name: "t", Role: models.RoleTrigger, Views: []models.EffectView{fetch("o-1")},
		ReqAt: now, ResAt: now.Add(time.Millisecond),
	})
	rec.OnMock(mctx, tr)
	mgr.AddMock(tr)
	e := consumerfake.Mock(consumerfake.MockOptions{
		Name: "e", Role: models.RoleEffect, Views: []models.EffectView{produce("o-1")},
		ReqAt: now.Add(10 * time.Millisecond), ResAt: now.Add(10 * time.Millisecond),
	})
	rec.OnMock(mctx, e)
	mgr.AddMock(e)

	cancel() // the recording is torn down before the test case is handed over
	stats := rec.Close(mctx)

	if stats.UnitsObserved != 1 {
		t.Fatalf("units observed: %d, want 1", stats.UnitsObserved)
	}
	if stats.UnitsPersisted != 0 {
		t.Fatalf("units persisted: %d, want 0 — nothing was handed over", stats.UnitsPersisted)
	}
	// COUNTED ONCE, BY NAME. The unit did not vanish: the recorder knows
	// what happened to it and says so. Counting it in UnitsRefused as well
	// as naming it is what keeps UnitsLost() — the safety net for a unit
	// nothing can account for — from reporting the same unit a second time
	// and printing "neither persisted nor refused by name" directly above
	// the by-name refusal for it.
	if stats.UnitsRefused != 1 {
		t.Fatalf("units refused: %d, want 1", stats.UnitsRefused)
	}
	if stats.UnitsLost() != 0 {
		t.Fatalf("units lost: %d, want 0 — this unit was accounted for by name, so counting it as lost too reports it twice (observed %d, persisted %d, refused %d)",
			stats.UnitsLost(), stats.UnitsObserved, stats.UnitsPersisted, stats.UnitsRefused)
	}
	err := stats.Err()
	if err == nil {
		t.Fatal("a unit that never reached persistence must fail the recording")
	}
	if !contains(err.Error(), string(models.CategoryConsumerUnitsLost)) {
		t.Fatalf("the failure must name CONSUMER_UNITS_LOST, got %q", err)
	}
	if contains(err.Error(), "neither persisted nor refused by name") {
		t.Fatalf("the reconciliation contradicts itself: it names the unit's refusal AND claims it was never refused by name — %q", err)
	}
}

// THE RECONCILIATION ARITHMETIC, PINNED DIRECTLY.
//
// UnitsLost() is the safety net for the one outcome §3 R6 forbids: a unit that
// was observed and then vanished with no name attached. Nothing in the
// recorder deliberately produces one — every path it knows about ends in a
// persist or a named refusal — so no end-to-end test can drive it, and without
// this table the whole lost-unit branch of Err() is unreachable from the
// suite. That is exactly the shape of a guard that quietly stops working.
func TestTheReconciliationCountsEveryUnitExactlyOnce(t *testing.T) {
	cases := []struct {
		name     string
		stats    consumer.RecorderStats
		wantLost int
		wantErr  bool
		wantIn   string
	}{{
		name:     "a clean recording",
		stats:    consumer.RecorderStats{UnitsObserved: 3, UnitsPersisted: 3},
		wantLost: 0,
	}, {
		name:     "every unit accounted for, some by name",
		stats:    consumer.RecorderStats{UnitsObserved: 3, UnitsPersisted: 2, UnitsRefused: 1, Refusals: []consumer.Refusal{{Units: []string{"u-2"}, Category: models.CategoryConsumerNoObservableEffect, Detail: "nothing observable"}}},
		wantLost: 0,
		wantErr:  true, // the named refusal still fails the recording
		wantIn:   string(models.CategoryConsumerNoObservableEffect),
	}, {
		name:     "a unit vanished with no name attached",
		stats:    consumer.RecorderStats{UnitsObserved: 3, UnitsPersisted: 2},
		wantLost: 1,
		wantErr:  true,
		wantIn:   "neither persisted nor refused by name",
	}, {
		name:     "the worker produced outside every unit",
		stats:    consumer.RecorderStats{UnitsObserved: 1, UnitsPersisted: 1, OrphanEffects: 2},
		wantLost: 0,
		wantErr:  true,
		wantIn:   "belong to no test",
	}, {
		// Defensive: over-counting must not wrap into a negative and
		// read as "nothing lost, nothing to see".
		name:     "more accounted for than observed",
		stats:    consumer.RecorderStats{UnitsObserved: 1, UnitsPersisted: 2},
		wantLost: 0,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stats.UnitsLost(); got != tc.wantLost {
				t.Fatalf("UnitsLost()=%d, want %d", got, tc.wantLost)
			}
			err := tc.stats.Err()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Err()=%v, want error: %v", err, tc.wantErr)
			}
			if tc.wantIn != "" && !contains(err.Error(), tc.wantIn) {
				t.Fatalf("Err()=%q, must mention %q", err, tc.wantIn)
			}
		})
	}
}

// Parsers call the recorder from their own goroutines, one per connection.
// This is a race-detector smoke test, not a behavioural one: it exists so a
// future field added outside the mutex is caught here rather than in a
// customer's recording.
func TestConcurrentParserGoroutinesDoNotRaceTheRecorder(t *testing.T) {
	r := newRig(t)
	base := r.clk.Now()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				at := base.Add(time.Duration(g*25+i) * time.Millisecond)
				r.rec.OnMock(r.ctx, consumerfake.Mock(consumerfake.MockOptions{
					Name: "e", Role: models.RoleEffect,
					Views: []models.EffectView{produce("o")},
					ReqAt: at, ResAt: at,
				}))
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			at := base.Add(time.Duration(i) * time.Second)
			r.rec.OnMock(r.ctx, consumerfake.Mock(consumerfake.MockOptions{
				Name: "t", Role: models.RoleTrigger,
				Views: []models.EffectView{fetch("o")},
				ReqAt: at, ResAt: at.Add(time.Millisecond), ConnID: "c1",
			}))
			r.rec.Stats()
		}
	}()
	// AND THE SIDE-EFFECT INGEST, WHICH ENTERS FROM THE OTHER DIRECTION.
	// Recorder.OnEgress runs INSIDE SyncMockManager.AddMock, with the
	// manager's mutex held, while onTrigger above takes the recorder's mutex
	// and then calls into the manager (SetFirstRequestSignaled, NextTestID,
	// DedupQueue().Enqueue). The lock order must be manager -> recorder in
	// both directions or this deadlocks; running the two concurrently is what
	// proves it, and -race proves the counter itself is guarded.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			at := base.Add(time.Duration(i) * time.Millisecond)
			r.mgr.AddMock(consumerfake.Mock(consumerfake.MockOptions{
				Name: "w", Kind: consumerfake.SideEffectKind,
				ReqAt: at, ResAt: at,
			}))
		}
	}()
	wg.Wait()
	r.rec.Close(r.ctx)
}

// CONSUME-AND-WRITE-TO-A-DATABASE MUST KEEP WORKING. It is one of the two most
// common consumer shapes: zero produced records, one database write. Refusing
// it for "no observable effect" would funnel-leak the feature, and counting the
// write toward the expected effects would hang every replay, because nothing
// on the replay side reports a database write to the gate.
func TestAConsumeAndWriteWorkerIsNotRefused(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	r.emit(r.sideEffect(r.at(20 * time.Millisecond)))
	r.rec.Close(r.ctx)

	tests := r.drainTests()
	if len(tests) != 1 {
		t.Fatalf("minted %d test cases, want 1", len(tests))
	}
	if got := tests[0].ConsumerSpec.Completion.ExpectEffects; got != 0 {
		t.Fatalf("expectEffects=%d, want 0: the gate cannot observe a database write, so counting one would time out every consume-and-write worker", got)
	}
	// AND THE COUNT MUST REACH THE FILE. `effects: []` with
	// `expectEffects: 0` is also what a unit in which NOTHING happened looks
	// like, and the judge refuses that shape as vacuous — so without this the
	// recorder mints a test the judge can only ever fail. See
	// TestARecorderMintedConsumeAndWriteTestPasses in pkg/service/replay.
	if got := tests[0].ConsumerSpec.SideEffects; got != 1 {
		t.Fatalf("sideEffects=%d, want 1: the judge cannot tell this from a unit where nothing happened without it", got)
	}
	if err := r.rec.Stats().Err(); err != nil {
		t.Fatalf("a consume-and-write recording must reconcile: %v", err)
	}
}

// A PRESENCE STAND-IN IS NOT COUNTED INTO ExpectEffects.
//
// A projector is free to return a presence view for a role=effect mock — it is
// a documented, first-class projector output, and it is exactly the shape
// design §2 describes for a database write. What the replay side can satisfy
// is produced RECORDS: Gate.pendingLocked deliberately excludes presence views
// from the observed count because nothing on the replay path calls
// ObserveEffect for a database write. Counting them here and not there makes
// the two ends disagree by construction — every such test burns its whole
// timeout and then fails EFFECT_MISSING with a bogus count row, 100% red on a
// healthy worker.
func TestAPresenceOnlyEffectViewIsNotCountedIntoExpectEffects(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	r.emit(r.effect(r.at(20*time.Millisecond), models.EffectView{
		Protocol: consumerfake.Protocol, Op: "write", Target: "orders",
		Decoded: models.DecodedPresence, Records: 1,
	}))
	r.rec.Close(r.ctx)

	tests := r.drainTests()
	if len(tests) != 1 {
		t.Fatalf("minted %d test cases, want 1: a unit whose only effect is a presence stand-in is still something the worker did, so it must not be refused as having no observable effect", len(tests))
	}
	spec := tests[0].ConsumerSpec
	if got := spec.Completion.ExpectEffects; got != 0 {
		t.Fatalf("expectEffects=%d, want 0: the gate never counts a presence view, so an expected count that includes one can never be reached", got)
	}
	if len(spec.Effects) != 1 || !spec.Effects[0].IsPresenceOnly() {
		t.Fatalf("the view itself must still be recorded, got %+v", spec.Effects)
	}
	if err := r.rec.Stats().Err(); err != nil {
		t.Fatalf("this recording must reconcile: %v", err)
	}
}

// A FAST WORKER MUST NOT MANUFACTURE AN ORPHAN.
//
// Closing a unit is not cheap: it resolves the dedup job — which walks the
// sync-mock buffer and forwards every matched mock through a channel — and
// then BLOCKS handing the minted test case to persistence. Effects arrive on a
// different connection and therefore a different parser goroutine, so a worker
// whose handler outruns that stretch genuinely races it. If the recorder holds
// no open unit across the close, that effect is attributed to no unit at all,
// and a SINGLE orphan record fails the entire recording — nondeterministically,
// on a recording that is otherwise perfect.
//
// The test-case channel here is unbuffered and drained only after the effect
// has been emitted, which pins the recorder in exactly that window.
func TestAnEffectProducedWhileTheRecorderIsMintingIsNotAnOrphan(t *testing.T) {
	t.Cleanup(consumerfake.Register())

	mgr := syncMock.New(zap.NewNop())
	mocks := make(chan *models.Mock, 256)
	mappings := make(chan models.TestMockMapping, 64)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr.SetOutputChannel(mocks)
	mgr.SetMappingChannel(ctx, mappings)

	// UNBUFFERED: closeUnit blocks on the send until something receives, which
	// is what holds the recorder inside the window under test.
	tests := make(chan *models.TestCase)
	clk := consumerfake.NewClock(time.Time{})
	rec := consumer.NewRecorder(consumer.RecorderOptions{
		Logger:        zap.NewNop(),
		Clock:         clk,
		TestCases:     tests,
		EnableMapping: true,
	})
	recCtx := syncMock.NewContext(ctx, mgr)

	now := clk.Now()
	at := func(d time.Duration) time.Time {
		now = now.Add(d)
		clk.Set(now)
		return now
	}
	emit := func(m *models.Mock) {
		rec.OnMock(recCtx, m)
		mgr.AddMock(m)
	}
	trig := func(reqAt, resAt time.Time, key string) *models.Mock {
		return consumerfake.Mock(consumerfake.MockOptions{
			Name: "kafka-fetch", Role: models.RoleTrigger, ConnID: "c1",
			Views: []models.EffectView{fetch(key)}, ReqAt: reqAt, ResAt: resAt,
		})
	}

	emit(trig(at(0), at(10*time.Millisecond), "o-1"))
	emit(consumerfake.Mock(consumerfake.MockOptions{
		Name: "kafka-produce", Role: models.RoleEffect,
		Views: []models.EffectView{produce("o-1")},
		ReqAt: at(20 * time.Millisecond), ResAt: now,
	}))

	// The second trigger. Its unit is opened and the FIRST unit is minted,
	// which blocks on the unbuffered channel above.
	t2req, t2res := at(300*time.Millisecond), at(10*time.Millisecond)
	triggerDone := make(chan struct{})
	go func() {
		defer close(triggerDone)
		emit(trig(t2req, t2res, "o-2"))
	}()

	// THE BARRIER. Resolving the previous unit's window is what emits its
	// mapping, and it happens BEFORE the blocking hand-off, so a mapping on
	// this channel means the recorder is now inside the mint. That is
	// precisely the stretch in which the recorder used to hold no open unit.
	select {
	case <-mappings:
	case <-time.After(testBudget):
		t.Fatal("the previous unit's window was never resolved")
	}

	// The worker produces the NEW message's effect while the recorder is
	// still finishing the previous unit.
	rec.OnMock(recCtx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "kafka-produce", Role: models.RoleEffect,
		Views: []models.EffectView{produce("o-2")},
		ReqAt: t2res.Add(time.Millisecond), ResAt: t2res.Add(2 * time.Millisecond),
	}))

	if got := rec.Stats().OrphanEffects; got != 0 {
		t.Fatalf("OrphanEffects = %d, want 0: the worker produced for the unit that was being opened, and attributing it to no unit at all fails an otherwise perfect recording", got)
	}

	// Let the blocked mint finish, then close the recording.
	minted := make(chan int, 1)
	go func() {
		n := 0
		for range tests {
			n++
		}
		minted <- n
	}()
	<-triggerDone
	rec.Close(recCtx)
	close(tests)

	if n := <-minted; n != 2 {
		t.Fatalf("minted %d test cases, want 2", n)
	}
	if err := rec.Stats().Err(); err != nil {
		t.Fatalf("this recording must reconcile: %v", err)
	}
}

// Same-protocol untagged traffic is the consumer's OWN coordination chatter
// (heartbeats, offset commits, metadata refreshes). Counting it as an effect
// would make the no-observable-effect refusal impossible to ever fire.
func TestSameProtocolCoordinationTrafficIsNotAnEffect(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	// A heartbeat: same Kind as the trigger, no role tag.
	r.emit(consumerfake.Mock(consumerfake.MockOptions{
		Name:  "kafka-heartbeat",
		ReqAt: r.at(20 * time.Millisecond),
		ResAt: r.now,
	}))
	r.rec.Close(r.ctx)

	if got := len(r.drainTests()); got != 0 {
		t.Fatalf("minted %d test cases; a unit whose only company was its own coordination traffic has nothing to assert", got)
	}
	stats := r.rec.Stats()
	if stats.UnitsRefused != 1 || stats.Refusals[0].Category != models.CategoryConsumerNoObservableEffect {
		t.Fatalf("want one no-observable-effect refusal, got %+v", stats.Refusals)
	}
}

// Dropping a projected view under-counts the expected effects, and an
// under-counted expectation turns a real over-production into a pass. So the
// bound refuses the unit instead.
func TestEffectCacheOverflowRefusesTheUnitInsteadOfDroppingAView(t *testing.T) {
	t.Cleanup(consumerfake.Register())
	mgr := syncMock.New(zap.NewNop())
	mocks := make(chan *models.Mock, 256)
	mgr.SetOutputChannel(mocks)
	ctx := syncMock.NewContext(context.Background(), mgr)
	tests := make(chan *models.TestCase, 8)
	clk := consumerfake.NewClock(time.Time{})
	rec := consumer.NewRecorder(consumer.RecorderOptions{
		Logger: zap.NewNop(), Clock: clk, TestCases: tests, MaxCachedViews: 2,
	})

	now := clk.Now()
	tr := consumerfake.Mock(consumerfake.MockOptions{
		Name: "t", Role: models.RoleTrigger, Views: []models.EffectView{fetch("o-1")},
		ReqAt: now, ResAt: now.Add(time.Millisecond),
	})
	rec.OnMock(ctx, tr)
	mgr.AddMock(tr)
	for i := 0; i < 4; i++ {
		at := now.Add(time.Duration(10+i) * time.Millisecond)
		e := consumerfake.Mock(consumerfake.MockOptions{
			Name: "e", Role: models.RoleEffect, Views: []models.EffectView{produce("o-1")},
			ReqAt: at, ResAt: at,
		})
		rec.OnMock(ctx, e)
		mgr.AddMock(e)
	}
	stats := rec.Close(ctx)

	if len(tests) != 0 {
		t.Fatalf("minted %d test cases; an overflowing unit must not become a file", len(tests))
	}
	if stats.UnitsRefused != 1 || stats.Refusals[0].Category != models.CategoryConsumerEffectCacheOverflow {
		t.Fatalf("want one cache-overflow refusal, got %+v", stats.Refusals)
	}
}

// A produced frame whose request began before this unit's window opened
// carries records the worker emitted while the PREVIOUS unit was open. v1
// refuses BOTH units by name rather than silently dropping one side or
// reporting a spurious extra.
func TestAStraddlingEffectRefusesBothUnitsByName(t *testing.T) {
	r := newRig(t)

	t1req, t1res := r.at(0), r.at(10*time.Millisecond)
	r.emit(r.trigger(t1req, t1res, "c1", fetch("o-1")))
	inUnitOne := r.at(20 * time.Millisecond)
	r.emit(r.effect(inUnitOne, produce("o-1")))

	t2req, t2res := r.at(300*time.Millisecond), r.at(10*time.Millisecond)
	r.emit(r.trigger(t2req, t2res, "c1", fetch("o-2")))
	// The producer's accumulator batched across the boundary: this frame
	// STARTED before unit two opened.
	straddling := consumerfake.Mock(consumerfake.MockOptions{
		Name: "kafka-produce", Role: models.RoleEffect,
		Views: []models.EffectView{produce("o-1"), produce("o-2")},
		ReqAt: t2res.Add(-5 * time.Millisecond),
		ResAt: r.at(20 * time.Millisecond),
	})
	r.emit(straddling)
	r.rec.Close(r.ctx)

	stats := r.rec.Stats()
	var found *consumer.Refusal
	for i := range stats.Refusals {
		if stats.Refusals[i].Category == models.CategoryConsumerEffectStraddlesUnit {
			found = &stats.Refusals[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no straddle refusal: %+v", stats.Refusals)
	}
	if len(found.Units) != 2 {
		t.Fatalf("a straddle must name BOTH units, got %v", found.Units)
	}
	if err := stats.Err(); err == nil {
		t.Fatal("a straddling frame must fail the recording")
	}
}

// A trigger stream that moved to another connection cannot have its recorded
// broker session identity replayed coherently.
func TestASecondTriggerConnectionRefusesTheRecording(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-1")))
	// A rebalance: the same protocol, a new connection.
	r.emit(r.trigger(r.at(300*time.Millisecond), r.at(10*time.Millisecond), "c2", fetch("o-2")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-2")))
	r.rec.Close(r.ctx)

	stats := r.rec.Stats()
	var seen bool
	for _, ref := range stats.Refusals {
		if ref.Category == models.CategoryConsumerMultiConnectionRecording {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("no multi-connection refusal: %+v", stats.Refusals)
	}
	if got := len(r.drainTests()); got != 1 {
		t.Fatalf("minted %d test cases, want only the one from the first connection", got)
	}
}

// Several sockets to the same broker cluster are normal — the group
// coordinator plus one per leader. Only a moved TRIGGER stream is refused;
// refusing on any second connection would refuse every real recording.
func TestOtherConnectionsCarryingNoTriggerAreNotAMultiConnectionRecording(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	// Coordination traffic on a different socket: the group coordinator,
	// or a second leader broker. A real consumer always has some.
	r.emit(consumerfake.Mock(consumerfake.MockOptions{
		Name: "kafka-heartbeat", ConnID: "c2",
		ReqAt: r.at(5 * time.Millisecond), ResAt: r.now,
	}))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-1")))
	// A second unit AFTER that other socket appeared. This is what makes
	// the test bite: the multi-connection flag is consumed when a unit
	// OPENS, so a rule that keyed on any second connection would refuse
	// this one.
	r.emit(r.trigger(r.at(300*time.Millisecond), r.at(10*time.Millisecond), "c1", fetch("o-2")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-2")))
	r.rec.Close(r.ctx)

	for _, ref := range r.rec.Stats().Refusals {
		if ref.Category == models.CategoryConsumerMultiConnectionRecording {
			t.Fatalf("a coordination socket was mistaken for a moved trigger stream: %v", ref)
		}
	}
	if got := len(r.drainTests()); got != 2 {
		t.Fatalf("minted %d test cases, want 2", got)
	}
}

// Effects produced while no unit is open belong to no test, so replay has
// nothing to compare them against. They are counted and they fail the
// recording, never silently discarded.
func TestEffectsProducedOutsideEveryUnitFailTheRecording(t *testing.T) {
	r := newRig(t)
	r.emit(r.effect(r.at(0), produce("o-0")))
	r.emit(r.trigger(r.at(10*time.Millisecond), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-1")))
	r.rec.Close(r.ctx)

	stats := r.rec.Stats()
	if stats.OrphanEffects != 1 {
		t.Fatalf("orphan effect records: %d, want 1", stats.OrphanEffects)
	}
	if err := stats.Err(); err == nil {
		t.Fatal("effects produced outside every unit must fail the recording")
	}
}

// A trigger the projector cannot describe cannot open a unit — and the unit it
// would have closed still has to close, because its window ends there whatever
// we can say about the frame.
func TestAnUndecodableTriggerIsRefusedByNameAndStillClosesTheOpenUnit(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-1")))

	badReq := r.at(300 * time.Millisecond)
	badRes := r.at(10 * time.Millisecond)
	bad := consumerfake.Mock(consumerfake.MockOptions{
		Name: "kafka-fetch", Role: models.RoleTrigger,
		Err:   "Fetch v13 is flexible and this decoder does not model it",
		ReqAt: badReq, ResAt: badRes,
	})
	r.emit(bad)

	// DRAINED BEFORE TEARDOWN, ON PURPOSE. Teardown closes whatever unit is
	// still open, so a recorder that did NOT close the healthy unit on the
	// undecodable frame would still mint its test a moment later and look
	// identical from the outside. Asserting here — and asserting that the
	// window ends at the BAD FRAME's response time — is what actually pins
	// the "still closes the open unit" half of this test's name. Without it
	// the whole close-on-undecodable-trigger path is unpinned.
	minted := r.drainTests()
	if len(minted) != 1 {
		t.Fatalf("minted %d test cases before teardown, want 1: the undecodable frame must close the healthy unit that was open, not leave it for teardown", len(minted))
	}
	if got := minted[0].ConsumerSpec.ResTimestampMock; !got.Equal(badRes) {
		t.Fatalf("the healthy unit's window ends at %s, want the undecodable frame's response time %s: the window has to end where the delivery stream said it ended, whatever we could decode of that frame", got, badRes)
	}

	r.rec.Close(r.ctx)
	if extra := r.drainTests(); len(extra) != 0 {
		t.Fatalf("teardown minted %d more test cases; an undecodable trigger opens no unit", len(extra))
	}
	stats := r.rec.Stats()
	var seen bool
	for _, ref := range stats.Refusals {
		if ref.Category == models.CategoryConsumerUnsupportedWireVersion {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("a declined trigger decode must be refused as an unsupported wire version, got %+v", stats.Refusals)
	}
}

// A projector crash is a keploy defect, not a wire-version limitation, and the
// two get different names so nobody files the bug as a limitation.
func TestAProjectorPanicIsRefusedUnderItsOwnCategory(t *testing.T) {
	r := newRig(t)
	bad := consumerfake.Mock(consumerfake.MockOptions{
		Name: "kafka-fetch", Role: models.RoleTrigger,
		Panic: "index out of range",
		ReqAt: r.at(0), ResAt: r.at(10 * time.Millisecond),
	})
	r.emit(bad)
	r.rec.Close(r.ctx)

	stats := r.rec.Stats()
	if len(stats.Refusals) != 1 || stats.Refusals[0].Category != models.CategoryConsumerProjectorFailed {
		t.Fatalf("want one CONSUMER_PROJECTOR_FAILED refusal, got %+v", stats.Refusals)
	}
}

// The minted test case must describe the SAME window the dedup queue just
// resolved, or regenerating the mapping from the test files would produce a
// different one.
func TestTheMintedWindowIsTheWindowTheQueueResolved(t *testing.T) {
	r := newRig(t)
	_, t1res := r.at(0), r.at(10*time.Millisecond)
	r.emit(r.trigger(r.now.Add(-10*time.Millisecond), t1res, "c1", fetch("o-1")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-1")))
	t2res := r.at(300 * time.Millisecond)
	r.emit(r.trigger(r.now.Add(-5*time.Millisecond), t2res, "c1", fetch("o-2")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-2")))
	r.rec.Close(r.ctx)

	tests := r.drainTests()
	if len(tests) < 1 {
		t.Fatal("no test cases")
	}
	spec := tests[0].ConsumerSpec
	if !spec.ReqTimestampMock.Equal(t1res) {
		t.Fatalf("window start %s, want the trigger's response time %s", spec.ReqTimestampMock, t1res)
	}
	if !spec.ResTimestampMock.Equal(t2res) {
		t.Fatalf("window end %s, want the next trigger's response time %s", spec.ResTimestampMock, t2res)
	}
	req, res := tests[0].RecordWindow()
	if !req.Equal(t1res) || !res.Equal(t2res) {
		t.Fatalf("RecordWindow returned (%s, %s)", req, res)
	}
	if tests[0].EarliestTimestamp().IsZero() {
		t.Fatal("a consumer test must have a non-zero earliest timestamp: it anchors generated TLS certificates on the RECORDING, and a zero one falls back to ca.go's wall-clock substitution — a certificate with no relationship to the exchange it stands in for")
	}
}

// The grace is derived from the recording rather than fixed, because the right
// drain is a property of the worker: a grace shorter than the worker's own
// latency reports its work as a missing effect.
func TestTheCompletionGraceIsDerivedAndClamped(t *testing.T) {
	r := newRig(t)
	res := r.at(10 * time.Millisecond)
	r.emit(r.trigger(r.now.Add(-10*time.Millisecond), res, "c1", fetch("o-1")))
	// A slow worker: the effect lands 900ms after delivery, so 3x that is
	// above the floor and below the ceiling.
	r.emit(r.effect(r.at(900*time.Millisecond), produce("o-1")))
	r.rec.Close(r.ctx)

	tests := r.drainTests()
	if len(tests) != 1 {
		t.Fatalf("minted %d test cases", len(tests))
	}
	got := tests[0].ConsumerSpec.Completion.GraceMs
	if got != 2000 {
		t.Fatalf("graceMs=%d; 900ms x3 clamped to the [%d,%d] range is %d",
			got, models.ConsumerGraceMinMs, models.ConsumerGraceMaxMs, models.ConsumerGraceMaxMs)
	}
	if tests[0].ConsumerSpec.Completion.TimeoutMs != models.ConsumerDefaultTimeoutMs {
		t.Fatalf("timeoutMs=%d", tests[0].ConsumerSpec.Completion.TimeoutMs)
	}
}

// A recorder with no manager on its context must not panic — a parser can be
// driven outside a recording session, and a nil receiver must be inert.
func TestTheRecorderIsInertWithoutAManagerOrARecorder(t *testing.T) {
	t.Cleanup(consumerfake.Register())
	var nilRec *consumer.Recorder
	nilRec.OnMock(context.Background(), consumerfake.Mock(consumerfake.MockOptions{Role: models.RoleTrigger}))
	nilRec.OnIdlePoll(context.Background(), time.Now())
	if stats := nilRec.Close(context.Background()); stats.UnitsObserved != 0 {
		t.Fatal("a nil recorder must be inert")
	}
	if consumer.RecorderFromContext(context.Background()) != nil {
		t.Fatal("an empty context must carry no recorder")
	}
	if consumer.GateFromContext(context.Background()) != nil {
		t.Fatal("an empty context must carry no gate")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// THE LANE COORDINATE HAS TO HAVE A PRODUCER. ConsumerSpec.OrderBy is the ONLY
// mechanism keeping "ordered within a lane, unordered across lanes" true, and
// with nothing writing it every recording gets the stricter one-lane-per-
// (protocol, target) reading — so two goroutines producing to different
// partitions of one topic interleave, get paired positionally, and are reported
// as a routing regression. That is exactly the false red the design says is
// handled.
//
// OSS must never choose the key, so it is read off the trigger mock's metadata,
// where the protocol recorder stamps it.
func TestOrderByIsReadOffTheTriggerAndReachesThePersistedSpec(t *testing.T) {
	r := newRig(t)
	tr := r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1"))
	tr.Spec.Metadata[models.MetaKeyOrderBy] = "partition"
	r.emit(tr)
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-1")))
	r.rec.Close(r.ctx)

	tests := r.drainTests()
	if len(tests) != 1 {
		t.Fatalf("minted %d test cases, want 1", len(tests))
	}
	if got := tests[0].ConsumerSpec.OrderBy; got != "partition" {
		t.Fatalf("orderBy=%q, want %q: without it the judge orders every effect to a target against every other", got, "partition")
	}
}

// A protocol that does not name a lane leaves it empty, and the judge then
// takes the stricter reading. A recording that did not claim its effects are
// independently ordered has not earned the weaker assertion.
func TestNoOrderByMetadataLeavesTheLaneUnnamed(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-1")))
	r.rec.Close(r.ctx)

	tests := r.drainTests()
	if len(tests) != 1 {
		t.Fatalf("minted %d test cases, want 1", len(tests))
	}
	if got := tests[0].ConsumerSpec.OrderBy; got != "" {
		t.Fatalf("orderBy=%q, want empty", got)
	}
}

// The count of side-effect calls has to reach the file, or the judge cannot
// tell a consume-and-write unit (which it must grade) from a unit in which
// nothing happened (which it must refuse). Both are `effects: []` with
// `expectEffects: 0` on those two fields alone.
func TestSideEffectsReachThePersistedSpec(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	r.emit(r.sideEffect(r.at(20 * time.Millisecond)))
	r.emit(r.sideEffect(r.at(30 * time.Millisecond)))
	r.rec.Close(r.ctx)

	tests := r.drainTests()
	if len(tests) != 1 {
		t.Fatalf("minted %d test cases, want 1", len(tests))
	}
	spec := tests[0].ConsumerSpec
	if spec.SideEffects != 2 {
		t.Fatalf("sideEffects=%d, want 2", spec.SideEffects)
	}
	if spec.Completion.ExpectEffects != 0 {
		t.Fatalf("expectEffects=%d: a database write is not something the gate can observe", spec.Completion.ExpectEffects)
	}
}

// THE SIDE-EFFECT INGEST IS THE MOCK CHOKE POINT, NOT A PARSER OBLIGATION.
//
// ConsumerSpec.SideEffects counts calls of a DIFFERENT protocol family made
// while a unit is open — the consume-and-write shape its own doc names, a
// Kafka -> MySQL or Kafka -> outgoing-HTTP worker. Those mocks are emitted by
// the mysql / http / generic parsers, none of which is consumer-aware or ever
// will be: nothing calls Recorder.OnMock for them, on any code path, in any
// deployment. Counted only from the consumer parser's own announcements the
// number is structurally ALWAYS ZERO, and closeUnit then refuses every healthy
// consume-and-write recording as CONSUMER_NO_OBSERVABLE_EFFECT — the whole
// recording fails and produces no test cases.
//
// So the ingest is registered on the syncMock manager, which is the one place
// every mock in the process passes (dns.go, http.go, mysql, generic, and
// supervisor.Session.EmitMock all end at AddMock). This test emits the write
// the way its real parser does — straight to AddMock, with the recorder never
// told — and requires the unit to survive with a count.
func TestASideEffectFromAParserThatNeverHeardOfTheContractIsStillCounted(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	r.emitFromAnotherParser(r.sideEffect(r.at(20 * time.Millisecond)))
	r.emitFromAnotherParser(r.sideEffect(r.at(30 * time.Millisecond)))
	r.rec.Close(r.ctx)

	tests := r.drainTests()
	if len(tests) != 1 {
		t.Fatalf("minted %d test cases, want 1: a consume-and-write worker whose writes come from another parser must not be refused as having no observable effect", len(tests))
	}
	if got := tests[0].ConsumerSpec.SideEffects; got != 2 {
		t.Fatalf("sideEffects=%d, want 2", got)
	}
}

// The other half: the choke point must NOT double-count what a cooperative
// consumer parser also announces through OnMock. A role-tagged mock reaches
// AddMock like any other, so without the role skip in OnEgress a parser that
// obeys OnMock's contract would have every one of its cross-family effects
// counted as a side effect as well.
func TestTheChokePointSkipsRoleTaggedMocks(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))

	// A role-tagged mock of ANOTHER protocol family, arriving at the choke
	// point exactly as the manager would hand it over. onOther's same-Kind
	// guard cannot save this one — the Kind differs — so the role skip is the
	// only thing standing between it and a double count.
	for _, role := range []string{models.RoleEffect, models.RoleTrigger} {
		r.rec.OnEgress(consumerfake.Mock(consumerfake.MockOptions{
			Name:  "postgres-write-tagged",
			Kind:  consumerfake.SideEffectKind,
			Role:  role,
			ReqAt: r.at(10 * time.Millisecond),
			ResAt: r.now,
		}))
	}
	// One genuinely untagged write, so the unit has something observable and
	// the count is proved to be exactly one rather than accidentally zero.
	r.emitFromAnotherParser(r.sideEffect(r.at(10 * time.Millisecond)))
	r.rec.Close(r.ctx)

	tests := r.drainTests()
	if len(tests) != 1 {
		t.Fatalf("minted %d test cases, want 1", len(tests))
	}
	if got := tests[0].ConsumerSpec.SideEffects; got != 1 {
		t.Fatalf("sideEffects=%d, want 1: a role-tagged mock is announced through OnMock, and counting it here too counts it twice", got)
	}
}

// A CANCELLED CONTEXT MUST NOT LOSE A UNIT THE CHANNEL COULD TAKE.
//
// closeUnit hands the minted test case over with a select on the channel and
// on ctx.Done(). When ctx is ALREADY cancelled and the channel has room BOTH
// cases are ready, and Go picks between ready cases uniformly at random — so
// the unit was refused as CONSUMER_UNITS_LOST about half the time even though
// the hand-off would have succeeded. onTrigger -> closeOpen -> closeUnit runs
// on the LIVE parser context during normal recording, so a Ctrl-C landing
// while a unit closes made the reconciliation non-deterministic, and
// CONSUMER_UNITS_LOST is precisely the category design §3 R6 says must never
// fire by accident.
//
// TWENTY UNITS, NOT ONE, and that is the whole design of this test: a coin
// flip passes a single-unit assertion half the time, which is what let the
// defect through in the first place. Twenty independent flips survive with
// probability 2^-20.
func TestAnAlreadyCancelledContextNeverLosesAUnitTheChannelCouldTake(t *testing.T) {
	t.Cleanup(consumerfake.Register())

	const units = 20
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(make(chan *models.Mock, 256))
	live, cancel := context.WithCancel(context.Background())
	ctx := syncMock.NewContext(live, mgr)
	cancel()

	clk := consumerfake.NewClock(time.Time{})
	minted := make(chan *models.TestCase, units+1)
	rec := consumer.NewRecorder(consumer.RecorderOptions{
		Logger: zap.NewNop(), Clock: clk, TestCases: minted,
	})

	now := clk.Now()
	for i := 0; i < units; i++ {
		now = now.Add(300 * time.Millisecond)
		clk.Set(now)
		rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
			Name: "trigger", Role: models.RoleTrigger, ConnID: "c-1",
			ReqAt: now, ResAt: now.Add(5 * time.Millisecond),
			Views: []models.EffectView{fetch("o-1")},
		}))
		now = now.Add(20 * time.Millisecond)
		clk.Set(now)
		rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
			Name: "effect", Role: models.RoleEffect, ConnID: "c-2",
			ReqAt: now, ResAt: now,
			Views: []models.EffectView{produce("o-1")},
		}))
	}
	stats := rec.Close(ctx)

	if got := len(minted); got != units {
		t.Fatalf("%d of %d units reached persistence with the context already cancelled and room in the channel; the hand-off is a coin flip", got, units)
	}
	if stats.UnitsPersisted != units || stats.UnitsLost() != 0 || stats.UnitsRefused != 0 {
		t.Fatalf("reconciliation says %+v; a unit the channel could take must never be reported lost", stats)
	}
}

// SIDE EFFECTS ARE BOUND TO THE UNIT'S WINDOW, which is the same authority the
// test-mock mapping uses.
//
// The ingest is the syncMock manager's kept stream (Recorder.OnEgress), so it
// carries all of the process's egress: a background cron INSERT, a
// long-running query that started before this message arrived. Counted
// unconditionally, such a call makes SideEffects non-zero for a unit that
// produced nothing — which is exactly what the mint refusal
// CONSUMER_NO_OBSERVABLE_EFFECT exists to catch, so ambient traffic could
// rescue a worker that silently drops every message (design §5 false-pass row
// 3). Binning on the window is what makes the count agree with what will
// actually land in mappings.yaml.
func TestACallThatBeganBeforeTheUnitOpenedIsNotItsSideEffect(t *testing.T) {
	r := newRig(t)
	// A background call already in flight when the message arrived.
	early := r.sideEffect(r.at(0))
	r.emit(r.trigger(r.at(10*time.Millisecond), r.at(20*time.Millisecond), "c1", fetch("o-1")))
	// It completes inside the unit's window, but it BEGAN outside it.
	r.emit(early)
	r.rec.Close(r.ctx)

	if tests := r.drainTests(); len(tests) != 0 {
		t.Fatalf("a unit whose only 'side effect' began before it opened has nothing observable and must be refused, got %d test case(s) with sideEffects=%d",
			len(tests), tests[0].ConsumerSpec.SideEffects)
	}
	stats := r.rec.Stats()
	if stats.UnitsRefused != 1 || stats.Refusals[0].Category != models.CategoryConsumerNoObservableEffect {
		t.Fatalf("want one no-observable-effect refusal, got %+v", stats.Refusals)
	}
}

// A UNIT KEPT ALIVE ONLY BY THE SIDE-EFFECT COUNT IS PERSISTED AND SAID OUT
// LOUD, because the count is not attribution and the replay judge will refuse
// exactly these tests by name (CONSUMER_NO_OBSERVABLE_EFFECT, rule 7) rather
// than pass them. Without the counter and the warning the decision is made
// here, at record time, and discovered a CI run later.
//
// A unit that produced a real, parser-attributed effect is NOT counted: that
// is the shape the judge can grade.
func TestAUnitWithNothingAttributedIsCountedAndWarnedAbout(t *testing.T) {
	t.Run("side-effect count only", func(t *testing.T) {
		r := newRig(t)
		r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
		r.emitFromAnotherParser(r.sideEffect(r.at(20 * time.Millisecond)))
		stats := r.rec.Close(r.ctx)

		if len(r.drainTests()) != 1 {
			t.Fatal("a consume-and-write unit is still persisted; the record is honest, the attribution is what is missing")
		}
		if stats.UnitsUnattributed != 1 {
			t.Fatalf("UnitsUnattributed=%d, want 1: a unit whose only claim is the side-effect count asserts nothing this build can check", stats.UnitsUnattributed)
		}
		if stats.Err() != nil {
			t.Fatalf("it must not FAIL the recording — the recording is faithful: %v", stats.Err())
		}
	})

	t.Run("an attributed effect is not counted", func(t *testing.T) {
		r := newRig(t)
		r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
		r.emit(r.effect(r.at(20*time.Millisecond), produce("o-1")))
		stats := r.rec.Close(r.ctx)

		if len(r.drainTests()) != 1 {
			t.Fatal("a consume-and-produce unit must be persisted")
		}
		if stats.UnitsUnattributed != 0 {
			t.Fatalf("UnitsUnattributed=%d, want 0: this unit's effect IS attributed and the judge can grade it", stats.UnitsUnattributed)
		}
	})
}

// A parser that records no request timestamp is not punished for it: the call
// is counted, exactly as it was before the window rule existed.
func TestASideEffectWithNoRecordedRequestTimeIsStillCounted(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	untimed := consumerfake.Mock(consumerfake.MockOptions{
		Name: "postgres-insert",
		Kind: consumerfake.SideEffectKind,
	})
	r.emit(untimed)
	r.rec.Close(r.ctx)

	tests := r.drainTests()
	if len(tests) != 1 {
		t.Fatalf("minted %d test cases, want 1", len(tests))
	}
	if got := tests[0].ConsumerSpec.SideEffects; got != 1 {
		t.Fatalf("sideEffects=%d, want 1", got)
	}
}

// A unit that produced protocol effects carries no side-effect count unless it
// also made calls of another family — the two are independent.
func TestSideEffectsIsZeroForAPureProducer(t *testing.T) {
	r := newRig(t)
	r.emit(r.trigger(r.at(0), r.at(10*time.Millisecond), "c1", fetch("o-1")))
	r.emit(r.effect(r.at(20*time.Millisecond), produce("o-1")))
	r.rec.Close(r.ctx)

	tests := r.drainTests()
	if len(tests) != 1 {
		t.Fatalf("minted %d test cases, want 1", len(tests))
	}
	if got := tests[0].ConsumerSpec.SideEffects; got != 0 {
		t.Fatalf("sideEffects=%d, want 0", got)
	}
}

// A recorder is created for EVERY recording session, including the
// overwhelming majority that never see a consumer protocol at all. Printing
// "0 units observed" at Info on every HTTP recording would be pure noise on a
// path this contract is meant not to touch.
func TestCloseIsSilentWhenNoConsumerUnitWasEverObserved(t *testing.T) {
	t.Cleanup(consumerfake.Register())
	core, logs := observer.New(zapcore.DebugLevel)
	rec := consumer.NewRecorder(consumer.RecorderOptions{
		Logger:    zap.New(core),
		Clock:     consumerfake.NewClock(time.Time{}),
		TestCases: make(chan *models.TestCase, 4),
	})

	stats := rec.Close(context.Background())
	if stats.UnitsObserved != 0 {
		t.Fatalf("precondition: %+v", stats)
	}
	if n := logs.FilterMessage("consumer recording reconciliation").Len(); n != 0 {
		t.Fatalf("an HTTP-only recording emitted %d consumer reconciliation lines", n)
	}
}

// And it must NOT be silent once a unit exists: the count is the number the
// agent loop reads, and a unit that was refused by name and a unit that
// vanished produce the same number of files.
func TestCloseReportsTheReconciliationOnceAUnitExists(t *testing.T) {
	t.Cleanup(consumerfake.Register())
	core, logs := observer.New(zapcore.DebugLevel)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(make(chan *models.Mock, 64))
	ctx := syncMock.NewContext(context.Background(), mgr)

	clk := consumerfake.NewClock(time.Time{})
	rec := consumer.NewRecorder(consumer.RecorderOptions{
		Logger: zap.New(core), Clock: clk, TestCases: make(chan *models.TestCase, 4),
	})
	base := clk.Now()
	rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "t", Role: models.RoleTrigger, ConnID: "c1",
		ReqAt: base, ResAt: base.Add(time.Millisecond),
		Views: []models.EffectView{fetch("o-1")},
	}))
	rec.Close(ctx)

	if n := logs.FilterMessage("consumer recording reconciliation").Len(); n != 1 {
		t.Fatalf("consumer reconciliation lines = %d, want 1", n)
	}
}
