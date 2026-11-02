package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

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
// mark with the run's manager's staging. A worker's call is marked on its
// own manager, so that cut does not apply to it: its miss is read by its
// test (red, never green), not dropped.
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
