package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// workerWindow is a test worker's misses window as the agent drives it
// (instanceExact): opened, taken back, read.
type workerWindow interface {
	workerMisses
	WithdrawTestErrorCapture()
}

// servedWorker serves test worker inst on p and returns its misses window.
func servedWorker(t *testing.T, p *Proxy, inst uint32) workerWindow {
	t.Helper()
	s, err := p.ServeInstance(inst)
	if err != nil {
		t.Fatal(err)
	}
	return s.(workerWindow)
}

// workerMiss queues a miss of the call made by test worker inst, as a
// parser's report reaches the proxy.
func workerMiss(p *Proxy, call string, inst uint32) {
	report := &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: call}
	p.sendMockNotFoundErrorFrom(models.NewMockMismatchError(models.ErrNoMockMatched, report), 0, inst, nil)
}

// readCalls reads r's misses as "call@instance", failing the test on an
// error.
func readCalls(t *testing.T, name string, r workerMisses) []string {
	t.Helper()
	got, err := r.GetMockErrors(context.Background())
	if err != nil {
		t.Fatalf("%s's read: %v", name, err)
	}
	calls := []string{}
	for _, m := range got {
		calls = append(calls, fmt.Sprintf("%s@%d", m.ActualSummary, m.Instance))
	}
	return calls
}

// The learn step follows the run's tests alone: a test worker's window
// opening, taken back or read neither begins nor ends the learn step's test.
func TestATestWorkersWindowLeavesTheLearnStepsTestAlone(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50081"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	w := servedWorker(t, p, 1)
	w.ContinueTestErrorCapture()
	if n := p.learnTracker.OpenEpoch(); n != 0 {
		t.Fatalf("worker 1's window opened the learn step's test %d", n)
	}
	p.ContinueTestErrorCapture()
	open := p.learnTracker.OpenEpoch()
	if open == 0 {
		t.Fatal("the run's window opened no learn step test")
	}
	w.WithdrawTestErrorCapture()
	if n := p.learnTracker.OpenEpoch(); n != open {
		t.Fatalf("worker 1's window taken back left the learn step's test %d, want the run's %d", n, open)
	}
	w.ContinueTestErrorCapture()
	readCalls(t, "worker 1", w)
	if n := p.learnTracker.OpenEpoch(); n != open {
		t.Fatalf("worker 1's read left the learn step's test %d, want the run's %d", n, open)
	}
	readCalls(t, "the run", p)
	if n := p.learnTracker.OpenEpoch(); n != 0 {
		t.Fatalf("the run's read left the learn step's test %d open", n)
	}
}

// A miss a test worker makes between its tests waits for that worker's next
// test, never the run's; and opening a worker's window leaves the run's
// open test, and the misses it holds, alone.
func TestAWorkersMissBetweenItsTestsIsCarriedIntoItsNextTestAlone(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50082"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	w := servedWorker(t, p, 1)
	w.ContinueTestErrorCapture()
	if got := readCalls(t, "worker 1", w); len(got) != 0 {
		t.Fatalf("worker 1's first test read %v, want nothing", got)
	}
	workerMiss(p, "GET /between", 1)
	flushDrain(p)
	p.ContinueTestErrorCapture()
	workerMiss(p, "GET /run", 0)
	flushDrain(p)
	w.ContinueTestErrorCapture()
	if got := readCalls(t, "worker 1", w); len(got) != 1 || got[0] != "GET /between@1" {
		t.Fatalf("worker 1's next test read %v, want only its miss made between its tests", got)
	}
	if got := readCalls(t, "the run", p); len(got) != 1 || got[0] != "GET /run@0" {
		t.Fatalf("the run's open test read %v, want only its own miss", got)
	}
}

// The set start's cut of a miss marked before the set's staging compares the
// mark with the staging of the manager it was marked on. A worker's call is
// marked on its own manager, so the run's manager's staging does not cut it:
// its miss is read by its test (red, never green), not dropped.
func TestAWorkersMarkedMissIsNotCutByTheRunsSetStaging(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50083"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	w := servedWorker(t, p, 1)
	p.setStaged.Store(5)
	marked := func(call string, inst uint32) {
		report := &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: call, Mark: models.RoundMarkedSettled, MarkEpoch: 1}
		p.sendMockNotFoundErrorFrom(models.NewMockMismatchError(models.ErrNoMockMatched, report), 0, inst, nil)
	}
	w.ContinueTestErrorCapture()
	p.ContinueTestErrorCapture()
	marked("GET /worker", 1)
	marked("GET /run", 0)
	if got := readCalls(t, "worker 1", w); len(got) != 1 || got[0] != "GET /worker@1" {
		t.Fatalf("worker 1's test read %v, want its marked miss", got)
	}
	if got := readCalls(t, "the run", p); len(got) != 0 {
		t.Fatalf("the run's test read %v, a miss marked before its set's staging", got)
	}
}

// A test worker's test's own late call (marked in o::TestX's view on the
// worker's manager) that keploy decides after o::TestX's end was read is kept
// at the next set's start for the worker's own next read, as the run's is:
// its report reaching the drain before the set's staging (the cut keeps it in
// the worker's window) or after it (the drain compares the mark with the
// worker's manager's staging), the worker's next read returns it as the
// earlier set's o::TestX's call, outside every test, so a next-set test of
// the same name is not charged with it; the run's read never returns it. A
// worker's call marked in the pool view before the staging and filed after
// it is cut, and said, as the run's is.
func TestAWorkersLateCallIsKeptForItsOwnNextReadAtTheNextSetsStart(t *testing.T) {
	for i, when := range []string{"before the staging", "after the staging"} {
		t.Run(when, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", "1")
			p, _ := storeProxy(t, ipv4Dest(t, fmt.Sprintf("127.0.0.1:%d", 50098+i)))
			core, logs := observer.New(zap.DebugLevel)
			p.logger = zap.New(core)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := exactMock("a", 0, models.LifetimeSession)
			if err := p.SetMocksWithWindow(ctx, nil, []*models.Mock{a}, models.BaseTime, time.Now()); err != nil {
				t.Fatal(err)
			}
			p.StartErrorDrain(ctx)
			w := servedWorker(t, p, 1)
			es, ok := w.(agent.ExactServer)
			if !ok {
				t.Fatal("worker 1's window is not its exact server")
			}
			m1 := instanceMgrOf(t, p, 1)
			m1.ServeExactPool(models.PoolKeyNone, nil)
			pool := &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /worker-pool"}
			integrations.StampMark(pool, m1.MarkCallRound())
			if err := es.ServeExactTest("o::TestX", []*models.Mock{a}); err != nil {
				t.Fatal(err)
			}
			w.ContinueTestErrorCapture()
			call := m1.MarkCallRound()
			if !call.InTest() || call.Test() != "o::TestX" {
				t.Fatalf("precondition: worker 1's call is not marked in o::TestX's view: in test %v, %q", call.InTest(), call.Test())
			}
			late := &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "POST /late"}
			integrations.StampMark(late, call)
			send := func(r *models.MockMismatchReport) {
				p.sendMockNotFoundErrorFrom(models.NewMockMismatchError(models.ErrNoMockMatched, r), 0, 1, nil)
				flushDrain(p)
			}
			if got := readCalls(t, "worker 1's o::TestX end", w); len(got) != 0 {
				t.Fatalf("precondition: o::TestX's end read %v", got)
			}
			if when == "before the staging" {
				send(late)
			}
			p.missCutPending.Store(true)
			if err := p.SetMocksWithWindow(ctx, nil, []*models.Mock{a}, models.BaseTime, time.Now()); err != nil {
				t.Fatal(err)
			}
			if when == "after the staging" {
				send(late)
			}
			send(pool)
			p.ContinueTestErrorCapture()
			if got := readCalls(t, "the run", p); len(got) != 0 {
				t.Fatalf("the run's next-set read %v; worker 1's call is no call of the run's", got)
			}
			w.ContinueTestErrorCapture()
			got, err := w.GetMockErrors(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].ActualSummary != "POST /late" || got[0].Instance != 1 || got[0].EarlierSetTest != "o::TestX" || got[0].BeforeSet || got[0].Kind != models.MissOutsideTest {
				t.Fatalf("worker 1's next-set first read %+v; want only o::TestX's late call, as the earlier set's o::TestX's, outside every test", got)
			}
			if n := logs.FilterMessage(missesCutAtSetStart).FilterField(zap.Strings("calls", []string{"GET /worker-pool"})).Len(); n != 1 {
				t.Fatalf("the cut of worker 1's pool view miss was said %d times; want once: %+v", n, logs.All())
			}
		})
	}
}

// A test worker's own late calls kept at the next set's start that its window
// has no room for make the worker's next read incomplete, naming the loss,
// and leave the run's read whole: the loss is the worker's. The window's
// state is set here: through the drain a window holds misses either in its
// open test's list or with none open, never in both, so the set start re-files
// no more than a window keeps.
func TestAWorkersLateCallsLostForRoomAtTheSetsStartAreItsNextReadsLoss(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50101"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	w := servedWorker(t, p, 1)
	named := func(i int) error {
		r := &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: fmt.Sprintf("POST /late/%d", i), MarkedTest: "o::TestX", MarkedInTest: true, Mark: models.RoundMarkedSettled, MarkEpoch: 1}
		return models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: models.ErrNoMockMatched, Instance: 1, MismatchReport: r, MissKind: models.MissInTest, At: time.Now()}
	}
	// Worker 1's last test's window, read, and its tail: one more of its
	// own calls than a window keeps.
	w1 := p.missWindowOf(1)
	acc := &testErrorAccumulator{read: true}
	p.captureMu.Lock()
	for i := 0; i < maxPendingMockErrors; i++ {
		acc.addBounded(named(i), maxPendingMockErrors)
	}
	w1.activeTestErrors.Store(acc)
	w1.pendingMockErrors.addBounded(named(maxPendingMockErrors), maxPendingMockErrors)
	p.captureMu.Unlock()
	p.cutMissesAtSetStart(ctx)
	p.ContinueTestErrorCapture()
	if got, err := p.GetMockErrors(ctx); err != nil || len(got) != 0 {
		t.Fatalf("the run's read %v %+v; want it whole and empty: the loss is worker 1's", err, got)
	}
	w.ContinueTestErrorCapture()
	got, err := w.GetMockErrors(ctx)
	if !errors.Is(err, models.ErrUnmatchedCallsUnread) || !strings.Contains(err.Error(), missLostBetweenFull) {
		t.Fatalf("worker 1's read returned %v; want it incomplete: %s", err, missLostBetweenFull)
	}
	if len(got) != maxPendingMockErrors {
		t.Fatalf("worker 1's read has %d calls; want the %d its window kept", len(got), maxPendingMockErrors)
	}
}

// With no error drain running, each miss waits unfiled in the proxy's queue
// and only the run's read sweeps it: a worker's read takes none of them, and
// says it is incomplete, as it cannot vouch for its own.
func TestAWorkersReadWithNoErrorDrainIsIncompleteAndTakesNoMiss(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50084"))
	if p.errDrainActive.Load() {
		t.Fatal("the error drain runs before it was started")
	}
	w := servedWorker(t, p, 1)
	w.ContinueTestErrorCapture()
	p.ContinueTestErrorCapture()
	workerMiss(p, "GET /run", 0)
	got, err := w.GetMockErrors(context.Background())
	if !errors.Is(err, models.ErrUnmatchedCallsUnread) || !strings.Contains(err.Error(), missUnfiledNoDrain) {
		t.Fatalf("worker 1's read with no error drain returned %v, want it incomplete: %s", err, missUnfiledNoDrain)
	}
	if len(got) != 0 {
		t.Fatalf("worker 1's read took %+v from the queue", got)
	}
	if got := readCalls(t, "the run", p); len(got) != 1 || got[0] != "GET /run@0" {
		t.Fatalf("the run's read %v, want its queued miss", got)
	}
}

// A lost miss makes only its own worker's read incomplete, naming the first
// cause: not the run's, and one of a worker keploy does not serve is no
// read's.
func TestAWorkersLostMissMakesOnlyItsOwnReadIncomplete(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50085"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	w := servedWorker(t, p, 1)
	w.ContinueTestErrorCapture()
	p.ContinueTestErrorCapture()
	for i := 0; i <= maxPendingMockErrors; i++ {
		workerMiss(p, fmt.Sprintf("GET /%d", i), 1)
	}
	if _, err := w.GetMockErrors(ctx); !errors.Is(err, models.ErrUnmatchedCallsUnread) || !strings.Contains(err.Error(), missLostListFull) {
		t.Fatalf("worker 1's read, its list overfull, returned %v; want it incomplete: %s", err, missLostListFull)
	}
	if got := readCalls(t, "the run", p); len(got) != 0 {
		t.Fatalf("the run's read %v, want nothing", got)
	}

	// No drain and no room: worker 2's miss is lost, and its read says so
	// first; that of worker 9, never served, is no read's.
	q, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50086"))
	q.errChannel = make(chan error)
	w2 := servedWorker(t, q, 2)
	w2.ContinueTestErrorCapture()
	q.ContinueTestErrorCapture()
	workerMiss(q, "GET /two", 2)
	workerMiss(q, "GET /nine", 9)
	if _, err := w2.GetMockErrors(ctx); err == nil || !strings.HasSuffix(err.Error(), missLostNoDrainChanFull) {
		t.Fatalf("worker 2's read, its miss lost, returned %v; want it incomplete first for: %s", err, missLostNoDrainChanFull)
	}
	if got := readCalls(t, "the run", q); len(got) != 0 {
		t.Fatalf("the run's read %v, want nothing", got)
	}
}

// A test worker's tail lost for room is said at the next set's start, as
// the run's is.
func TestAWorkersTailLostForRoomIsSaidAtTheSetsStart(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50087"))
	core, logs := observer.New(zap.WarnLevel)
	p.logger = zap.New(core)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	servedWorker(t, p, 1)
	for i := 0; i <= maxPendingMockErrors; i++ {
		workerMiss(p, fmt.Sprintf("GET /%d", i), 1)
	}
	flushDrain(p)
	p.cutMissesAtSetStart(ctx)
	if n := logs.FilterMessageSnippet("than keploy keeps; the rest were dropped").Len(); n != 1 {
		t.Fatalf("the set start said %d times that worker 1's tail was dropped for room, want once", n)
	}
	if w := p.missWindowOf(1); w.pendingLost {
		t.Fatal("worker 1's loss outlived the set start that said it")
	}
}

// The run's tail lost for room is said at the next set's start beside a test
// worker whose tail lost none, and a loss the run's window or a worker's
// recorded since the previous set's last read leaves no read of the new set
// short: it was of the previous set's tail.
func TestARunsTailLostForRoomIsSaidBesideAWorkersAtTheSetsStart(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50094"))
	core, logs := observer.New(zap.WarnLevel)
	p.logger = zap.New(core)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	servedWorker(t, p, 1)
	p.captureMu.Lock()
	p.pendingLost = true
	p.captureMu.Unlock()
	p.missLoss.Store(&missLostListFull)
	p.missWindowOf(1).missLoss.Store(&missLostListFull)
	p.cutMissesAtSetStart(ctx)
	if n := logs.FilterMessageSnippet("than keploy keeps; the rest were dropped").Len(); n != 1 {
		t.Fatalf("the set start said %d times that the run's tail was dropped for room, want once", n)
	}
	if p.missLoss.Load() != nil || p.missWindowOf(1).missLoss.Load() != nil {
		t.Fatalf("a loss of the previous set's tail outlived the set start: the run's %v, worker 1's %v", p.missLoss.Load() != nil, p.missWindowOf(1).missLoss.Load() != nil)
	}
}

// A worker's miss says whether its test was open from the worker's own
// manager, not the run's.
func TestAWorkersMissIsKindedByItsOwnManager(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50088"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	w := servedWorker(t, p, 1)
	if err := w.(interface {
		ServeExactTest(string, []*models.Mock) error
	}).ServeExactTest("T", nil); err != nil {
		t.Fatal(err)
	}
	w.ContinueTestErrorCapture()
	workerMiss(p, "GET /in", 1)
	got, err := w.GetMockErrors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != models.MissInTest {
		t.Fatalf("worker 1's miss in its open test read %+v, want one of kind %q", got, models.MissInTest)
	}
}

// missParser misses every call it is handed.
type missParser struct{}

func (missParser) MatchType(context.Context, []byte) bool { return true }
func (missParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (missParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /parsed"})
}

// A miss a parser returns on a worker's connection is that worker's, as the
// kernel stamped the connection: its test reads it, the run's does not.
func TestAWorkersParserMissIsFiledUnderTheWorkerTheKernelStamped(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	dest := ipv4Dest(t, "127.0.0.1:50089")
	dest.KernelPid, dest.Instance = uint32(os.Getpid()), 5
	p, _ := instanceProxy(t, dest, nil)
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: missParser{}}
	t.Cleanup(p.endInstances)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	w := servedWorker(t, p, 5)
	w.ContinueTestErrorCapture()
	p.ContinueTestErrorCapture()
	_ = handle(t, p)
	if got := readCalls(t, "worker 5", w); len(got) != 1 || got[0] != "GET /parsed@5" {
		t.Fatalf("worker 5's test read %v, want its connection's miss", got)
	}
	if got := readCalls(t, "the run", p); len(got) != 0 {
		t.Fatalf("the run's test read %v, a worker's miss", got)
	}
}

// A miss of a test worker keploy had stopped serving as the call missed (a
// process the worker left, on a connection it made before its end) is no
// open test's to read, and is counted as a call keploy cannot attribute,
// naming the ended worker, as record charges it. One made while the worker
// was served, a miss of the run's, and one of an instance while no worker
// is served are not.
func TestAStoppedWorkersMissIsCountedACallKeployCannotAttribute(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50079"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	unattributed := func() (n uint64, what string) {
		for _, l := range p.CaptureLosses() {
			if l.Kind == models.CaptureLossUnattributed {
				n, what = n+l.Count, l.What
			}
		}
		return n, what
	}
	miss := func(inst uint32, report *models.MockMismatchReport, dst *models.ConditionalDstCfg) {
		err := models.NewMockMismatchError(models.ErrNoMockMatched, report)
		if report == nil {
			err = models.ErrNoMockMatched
		}
		p.sendMockNotFoundErrorFrom(err, 4242, inst, dst)
	}
	miss(7, &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /none"}, nil)
	if n, _ := unattributed(); n != 0 {
		t.Fatalf("a miss of an instance while keploy serves no worker counted %d calls keploy cannot attribute", n)
	}
	s1, err := p.ServeInstance(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ServeInstance(2); err != nil {
		t.Fatal(err)
	}
	w1 := s1.(workerMisses)
	w1.ContinueTestErrorCapture()
	miss(2, &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /served"}, nil)
	miss(0, &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /run"}, nil)
	if err := p.StopServingInstance(2); err != nil {
		t.Fatal(err)
	}
	if n, _ := unattributed(); n != 0 {
		t.Fatalf("a miss made while its worker was served, and the run's, counted %d calls keploy cannot attribute", n)
	}
	miss(2, &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /late", Destination: "orders:8080"}, nil)
	if n, what := unattributed(); n != 1 || what != "the latest: a call to orders:8080, from process 4242 of ended test worker 2, on a connection it made before keploy stopped serving that worker" {
		t.Fatalf("a stopped worker's miss counted %d, %q; want 1 naming its destination, process and worker", n, what)
	}
	miss(2, nil, &models.ConditionalDstCfg{Addr: "10.0.0.9:5432"})
	if n, what := unattributed(); n != 2 || what != "the latest: a call to 10.0.0.9:5432, from process 4242 of ended test worker 2, on a connection it made before keploy stopped serving that worker" {
		t.Fatalf("a stopped worker's miss with no report counted %d, %q; want 2 naming its dialled address", n, what)
	}
	miss(2, nil, &models.ConditionalDstCfg{Addr: "10.0.0.9:5432", AddrFabricated: true})
	if n, what := unattributed(); n != 3 || what != "the latest: a call, from process 4242 of ended test worker 2, on a connection it made before keploy stopped serving that worker" {
		t.Fatalf("a stopped worker's miss to a made-up address counted %d, %q; want 3 naming no address", n, what)
	}
	miss(2, &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /nodest"}, &models.ConditionalDstCfg{Addr: "10.0.0.9:5432"})
	if n, what := unattributed(); n != 4 || what != "the latest: a call to 10.0.0.9:5432, from process 4242 of ended test worker 2, on a connection it made before keploy stopped serving that worker" {
		t.Fatalf("a stopped worker's miss whose report names no destination counted %d, %q; want 4 naming its dialled address", n, what)
	}
	miss(2, nil, &models.ConditionalDstCfg{})
	if n, what := unattributed(); n != 5 || what != "the latest: a call, from process 4242 of ended test worker 2, on a connection it made before keploy stopped serving that worker" {
		t.Fatalf("a stopped worker's miss with no dialled address counted %d, %q; want 5 naming no address", n, what)
	}
	miss(2, nil, nil)
	if n, what := unattributed(); n != 6 || what != "the latest: a call, from process 4242 of ended test worker 2, on a connection it made before keploy stopped serving that worker" {
		t.Fatalf("a stopped worker's miss with neither a report nor a destination counted %d, %q; want 6 naming no address", n, what)
	}
	miss(2, &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /nowhere"}, &models.ConditionalDstCfg{Addr: "10.0.0.9:5432", AddrFabricated: true})
	if n, what := unattributed(); n != 7 || what != "the latest: a call, from process 4242 of ended test worker 2, on a connection it made before keploy stopped serving that worker" {
		t.Fatalf("a stopped worker's miss whose report names no destination, to a made-up address, counted %d, %q; want 7 naming no address", n, what)
	}
	// A later claim of the id (a worker that took id 2 after the first
	// ended) is named as that claim, never by its raw value.
	later := agent.InstanceValue(2, 1)
	if _, err := p.ServeInstance(later); err != nil {
		t.Fatal(err)
	}
	if err := p.StopServingInstance(later); err != nil {
		t.Fatal(err)
	}
	miss(later, &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /later", Destination: "orders:8080"}, nil)
	if n, what := unattributed(); n != 8 || what != "the latest: a call to orders:8080, from process 4242 of ended test worker 2 (claim 2 of that id), on a connection it made before keploy stopped serving that worker" {
		t.Fatalf("a stopped later claim's miss counted %d, %q; want 8 naming the claim", n, what)
	}
	if p.stoppedWorker(0) {
		t.Fatal("instance 0, no worker's, was read as a stopped worker's")
	}
	got, err := w1.GetMockErrors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("worker 1's open test read %v, a stopped worker's misses", got)
	}
}
