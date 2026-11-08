//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// instanceWire is a wireProxy that serves each test worker from a manager of
// its own (coreAgent.InstanceServer): each worker's is a wireProxy too, and
// the run's is the embedded one.
type instanceWire struct {
	*wireProxy
	imu     sync.Mutex
	workers map[uint32]*workerWire
	served  []uint32 // each ServeInstance, in order
	stopped []uint32 // each StopServingInstance, in order
	// serveErr fails ServeInstance; poolErr fails the pool serves of each
	// worker served from then on.
	serveErr, poolErr error
}

// workerWire is a worker's manager: readOpen says its test's outcome was
// read while the test was still served; gave holds the values its tests
// gave, bound on it.
type workerWire struct {
	*wireProxy
	readOpen bool
	gave     []models.VariableBinding
}

func (w *workerWire) GiveExactTest(test string, b models.VariableBinding) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if test != w.exactTest {
		return fmt.Errorf("%s is not the test this manager serves now", test)
	}
	w.gave = append(w.gave, b)
	return nil
}

func (w *workerWire) ExactTestOutcome() (string, []string, []string, bool) {
	w.mu.Lock()
	if w.exactTest != "" {
		w.readOpen = true
	}
	w.mu.Unlock()
	return w.wireProxy.ExactTestOutcome()
}

func (p *instanceWire) ServeInstance(inst uint32) (coreAgent.ExactServer, error) {
	p.imu.Lock()
	defer p.imu.Unlock()
	p.served = append(p.served, inst)
	if p.serveErr != nil {
		return nil, p.serveErr
	}
	w := &workerWire{wireProxy: &wireProxy{served: map[string]uint64{}, setErr: p.poolErr}}
	if p.workers == nil {
		p.workers = map[uint32]*workerWire{}
	}
	p.workers[inst] = w
	return w, nil
}

func (p *instanceWire) StopServingInstance(inst uint32) error {
	p.imu.Lock()
	defer p.imu.Unlock()
	p.stopped = append(p.stopped, inst)
	if _, ok := p.workers[inst]; !ok {
		return errors.New("not served")
	}
	delete(p.workers, inst)
	return nil
}

func (p *instanceWire) worker(t *testing.T, inst uint32) *workerWire {
	t.Helper()
	p.imu.Lock()
	defer p.imu.Unlock()
	w := p.workers[inst]
	require.NotNil(t, w, "test worker %d is not served", inst)
	return w
}

func (p *instanceWire) log() (served, stopped []uint32) {
	p.imu.Lock()
	defer p.imu.Unlock()
	return append([]uint32(nil), p.served...), append([]uint32(nil), p.stopped...)
}

// instanceAgent is workerAgent in mode, whose proxy serves each worker from
// a manager of its own.
func instanceAgent(t *testing.T, mode models.Mode) (*Agent, *instanceWire, *instanceHooks) {
	t.Helper()
	a, h := workerAgent(t, mode, 4)
	p := &instanceWire{wireProxy: a.Proxy.(*wireProxy)}
	a.Proxy = p
	return a, p, h
}

// claimAsWorker claims the run for process pid as one of two test workers,
// under the wire, as ClaimInstance does past its refusal of record's full
// round's workers (learnWorkersRefusal).
func claimAsWorker(a *Agent, pid int, key string) (models.InstanceResp, *models.ScopeError) {
	ctx := context.Background()
	if err := a.wire.lock(ctx); err != nil {
		return models.InstanceResp{}, abandoned(err)
	}
	defer a.wire.unlock()
	start, _ := procStart(pid)
	stamper, _ := a.Hooks.(coreAgent.InstanceStamper)
	return a.claimWorker(ctx, models.InstanceReq{Key: key, Dir: workerDir(key), Workers: 2}, claimant{pid: pid, start: start}, 2, stamper)
}

// In a replay each worker's claim serves it from a manager of its own, armed
// with its own key's pool slice; the run's manager is served none of them.
// A record serves no worker from a manager of its own.
func TestAReplayWorkerIsServedItsOwnKeysPoolFromItsOwnManager(t *testing.T) {
	a, p, _ := instanceAgent(t, models.MODE_TEST)
	a.proven.pool = map[string][]string{"a": {"pa"}, "b": {"pb"}}
	w1, w2 := child(t), child(t)
	resp, serr := claimAsWorker(a, w1.Process.Pid, "a")
	require.Nil(t, serr)
	require.Equal(t, "1", resp.Instance)
	resp, serr = claimAsWorker(a, w2.Process.Pid, "b")
	require.Nil(t, serr)
	require.Equal(t, "2", resp.Instance)
	served, _ := p.log()
	require.Equal(t, []uint32{1, 2}, served)
	require.Equal(t, []string{"pa"}, p.worker(t, 1).exactNames)
	require.Equal(t, []string{"pb"}, p.worker(t, 2).exactNames)
	require.Nil(t, p.exactNames, "the run's manager serves no worker's pool")

	r, rp, _ := instanceAgent(t, models.MODE_RECORD)
	_, serr = claimAsWorker(r, w1.Process.Pid, "a")
	require.Nil(t, serr)
	require.NoError(t, r.resetSession(context.Background()))
	served, stopped := rp.log()
	require.Empty(t, served, "a record serves no worker")
	require.Empty(t, stopped, "a record stops serving no worker")
}

// A replay worker's claim whose proxy cannot serve it from a manager of its
// own, or fails to, or cannot arm its pool, is refused, named: it is no
// worker, its instance ends, and a manager it was served stops.
func TestAReplayWorkerKeployCannotServeOnItsOwnIsRefused(t *testing.T) {
	a, h := workerAgent(t, models.MODE_TEST, 4)
	_, serr := claimAsWorker(a, os.Getpid(), "a")
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInInstance, serr.Code)
	require.Contains(t, serr.Message, "cannot serve each test worker's calls from mocks of its own")
	require.Empty(t, a.proven.workers)
	require.Equal(t, []uint32{1}, h.endedIDs())

	b, p, hb := instanceAgent(t, models.MODE_TEST)
	p.serveErr = errors.New("no session")
	_, serr = claimAsWorker(b, os.Getpid(), "a")
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrInternal, serr.Code)
	require.Contains(t, serr.Message, "could not serve test worker process")
	require.Contains(t, serr.Message, "no session")
	require.Empty(t, b.proven.workers)
	require.Equal(t, []uint32{1}, hb.endedIDs())

	c, pc, hc := instanceAgent(t, models.MODE_TEST)
	pc.poolErr = errors.New("the pool is gone")
	_, serr = claimAsWorker(c, os.Getpid(), "a")
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrInternal, serr.Code)
	require.Contains(t, serr.Message, "could not arm the recorded calls of a made outside its tests")
	require.Empty(t, c.proven.workers)
	require.Empty(t, c.proven.claims, "a refused claim is in no claim row")
	_, stopped := pc.log()
	require.Equal(t, []uint32{1}, stopped)
	require.Equal(t, []uint32{1}, hc.endedIDs())
}

// A replay worker's test is served, read and ended on its worker's manager
// alone: the run's and the other worker's are not touched, and the end
// stops serving the test before it reads it, then serves the worker its own
// key's pool again.
func TestAReplayWorkersTestIsServedAndReadOnItsWorkersManager(t *testing.T) {
	a, p, _ := instanceAgent(t, models.MODE_TEST)
	a.proven.pool = map[string][]string{"a": {"pa"}, "b": {"pb"}, models.PoolKeyNone: {"pnone"}}
	w1, w2 := child(t), child(t)
	_, serr := claimAsWorker(a, w1.Process.Pid, "a")
	require.Nil(t, serr)
	_, serr = claimAsWorker(a, w2.Process.Pid, "b")
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	require.Equal(t, "b::TestTwo", p.worker(t, 2).exactTest)
	require.Equal(t, []string{"pa"}, p.worker(t, 1).exactNames, "the other worker is still served its pool")
	require.Empty(t, p.lastTest, "the run's manager serves no worker's test")
	require.False(t, a.servedSince("b::TestTwo").IsZero())
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Equal(t, "", p.worker(t, 2).exactTest)
	require.Equal(t, []string{"pb"}, p.worker(t, 2).exactNames)
	require.False(t, p.worker(t, 2).readOpen, "the test's outcome was read while it was still served")
	require.Nil(t, p.exactNames, "the end served the run's manager a pool")
	require.Equal(t, models.ScopeOutcomePassed, windowOf(t, a, "b::TestTwo").Outcome)
}

// In a replay a value a worker's test gives is bound on the worker's own
// manager, which serves the test; the other worker's holds none.
func TestAReplayWorkersGivenIsBoundOnItsWorkersManager(t *testing.T) {
	a, p, _ := instanceAgent(t, models.MODE_TEST)
	a.proven.pool = map[string][]string{"a": {"pa"}, "b": {"pb"}}
	w1, w2 := child(t), child(t)
	_, serr := claimAsWorker(a, w1.Process.Pid, "a")
	require.Nil(t, serr)
	_, serr = claimAsWorker(a, w2.Process.Pid, "b")
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	resp, serr := a.ProvenGive(as(w2.Process.Pid), give("app", "app-1", "b_test.go:9"))
	require.Nil(t, serr)
	require.Equal(t, models.GivenVariable("app", 1), resp.Variable)
	gave := p.worker(t, 2).gave
	require.Len(t, gave, 1)
	require.Equal(t, "app-1", gave[0].Value)
	require.Equal(t, "test b::TestTwo", gave[0].Mock)
	require.Empty(t, p.worker(t, 1).gave, "the other worker's manager holds the value")
}

// In a replay a worker's test opens its worker's misses window and is charged
// the misses of its worker's processes alone, read on its worker's manager:
// the run's, and the other worker's, are not its.
func TestAReplayWorkersTestIsChargedOnlyItsWorkersMisses(t *testing.T) {
	a, p, _ := instanceAgent(t, models.MODE_TEST)
	a.proven.pool = map[string][]string{"a": {"pa"}, "b": {"pb"}}
	w1, w2 := child(t), child(t)
	_, serr := claimAsWorker(a, w1.Process.Pid, "a")
	require.Nil(t, serr)
	_, serr = claimAsWorker(a, w2.Process.Pid, "b")
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	one, two := p.worker(t, 1), p.worker(t, 2)
	require.Equal(t, 1, two.continues, "worker 2's test opened no window of its worker's")
	require.Zero(t, one.continues, "worker 2's test opened worker 1's window")
	require.Zero(t, p.continues, "a worker's test opened the run's window")
	one.mu.Lock()
	one.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /one", Instance: 1}}
	one.mu.Unlock()
	two.mu.Lock()
	two.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /two", Instance: 2}}
	two.mu.Unlock()
	p.mu.Lock()
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /run"}}
	p.mu.Unlock()

	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	w := windowOf(t, a, "b::TestTwo")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Len(t, w.Evidence.Misses, 1)
	require.Equal(t, "GET /two", w.Evidence.Misses[0].ActualSummary)
	one.mu.Lock()
	require.Len(t, one.misses, 1, "worker 2's test read worker 1's misses")
	one.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Len(t, p.misses, 1, "a worker's test read the run's misses")
}

// A worker's test whose proxy cannot read its worker's misses apart is not
// judged on the run's: its read fails, named.
func TestAReplayWorkersMissesThatCannotBeReadApartFailTheRead(t *testing.T) {
	a, _, _ := instanceAgent(t, models.MODE_TEST)
	a.provenMu.Lock()
	a.proven.workers = map[uint32]workerClaim{3: {es: opaqueServer{}}}
	a.provenMu.Unlock()
	_, err := a.missesOf(context.Background(), 3)
	require.ErrorContains(t, err, "cannot read test worker 3's misses apart")
	_, err = a.missesOf(context.Background(), 4)
	require.ErrorContains(t, err, "does not serve test worker 4")
}

// opaqueServer is a worker's manager that reads no misses of its own.
type opaqueServer struct{ coreAgent.ExactServer }

// open, each its own key's: one with a test open keeps the test served.
func TestTheCLIsPoolReachesEachIdleReplayWorker(t *testing.T) {
	a, p, _ := instanceAgent(t, models.MODE_TEST)
	w1, w2 := child(t), child(t)
	_, serr := claimAsWorker(a, w1.Process.Pid, "a")
	require.Nil(t, serr)
	_, serr = claimAsWorker(a, w2.Process.Pid, "b")
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	require.NoError(t, a.SetScopePool(context.Background(), map[string][]string{"a": {"pa"}, "b": {"pb"}}))
	require.Equal(t, "a::TestOne", p.worker(t, 1).exactTest, "a worker's open test stays served")
	require.Equal(t, []string{"pb"}, p.worker(t, 2).exactNames)
	require.Nil(t, p.exactNames, "the run's manager serves no worker's pool")
}

// A new session stops serving every replay worker's manager.
func TestANewSessionStopsServingEachReplayWorker(t *testing.T) {
	a, p, h := instanceAgent(t, models.MODE_TEST)
	w1 := child(t)
	_, serr := claimAsWorker(a, w1.Process.Pid, "a")
	require.Nil(t, serr)
	_, serr = claimAsWorker(a, os.Getpid(), "b")
	require.Nil(t, serr)
	require.NoError(t, a.resetSession(context.Background()))
	_, stopped := p.log()
	require.Equal(t, []uint32{1, 2}, stopped)
	require.Equal(t, []uint32{1, 2}, h.endedIDs())
}

// A replay worker that exits with a test open fails the test, named: what
// its manager served is no longer read as a live worker's.
func TestAGoneReplayWorkersOpenTestFails(t *testing.T) {
	a, p, h := instanceAgent(t, models.MODE_TEST)
	gone := child(t)
	_, serr := claimAsWorker(a, gone.Process.Pid, "a")
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(gone.Process.Pid), begin("TestCrash"))
	require.Nil(t, serr)
	kill(t, gone)
	ctx := context.Background()
	require.NoError(t, a.wire.lock(ctx))
	a.endGoneWorkers(ctx)
	a.wire.unlock()
	_, stopped := p.log()
	require.Equal(t, []uint32{1}, stopped)
	require.Equal(t, []uint32{1}, h.endedIDs())
	w := windowOf(t, a, "a::TestCrash")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Contains(t, w.Evidence.Reason, "the test worker's process exited while the test was open")
}

// A gone replay worker's id, claimed again, is served under the id's next
// generation: the proxy stops serving the gone worker's value and never
// serves it again, so a call on a connection its processes left open is
// refused (charged as unattributed), never answered from, or filed as a miss
// of, the next worker's test.
func TestAGoneReplayWorkersValueIsNotTheNextClaimsOfItsId(t *testing.T) {
	a, p, _ := instanceAgent(t, models.MODE_TEST)
	gone := child(t)
	first, serr := claimAsWorker(a, gone.Process.Pid, "a")
	require.Nil(t, serr)
	kill(t, gone)
	ctx := context.Background()
	require.NoError(t, a.wire.lock(ctx))
	a.endGoneWorkers(ctx)
	a.wire.unlock()
	next, serr := claimAsWorker(a, os.Getpid(), "b")
	require.Nil(t, serr)
	require.Equal(t, []string{v(1, 0), v(1, 1)}, []string{first.Instance, next.Instance})
	served, stopped := p.log()
	require.Equal(t, []uint32{coreAgent.InstanceValue(1, 0), coreAgent.InstanceValue(1, 1)}, served)
	require.Equal(t, []uint32{coreAgent.InstanceValue(1, 0)}, stopped, "the gone worker's value stays unserved")
}

// lossWire is an instanceWire whose proxy counts the calls it refused, as
// the proxy does each connection of no served worker's.
type lossWire struct {
	*instanceWire
	lmu    sync.Mutex
	losses models.CaptureLosses
}

func (p *lossWire) CaptureLosses() models.CaptureLosses {
	p.lmu.Lock()
	defer p.lmu.Unlock()
	return append(models.CaptureLosses(nil), p.losses...)
}

// refuse counts the proxy's n-th refused call so far.
func (p *lossWire) refuse(n uint64) {
	p.lmu.Lock()
	defer p.lmu.Unlock()
	p.losses = models.CaptureLosses{{Kind: models.CaptureLossUnattributed, Count: n, What: fmt.Sprintf("the latest: a connection to 10.0.0.9:5432, from process %d, in no test worker's keploy instance", 4000+n)}}
}

// A call a replay of test workers refused is charged to the test open when
// it was made, and leaves it without a verdict; one made while no test was
// open is the next test's, whichever worker's. A test that began after a
// refusal was charged is not charged it again.
func TestAReplaysRefusedCallIsChargedToTheTestOpenThenAlone(t *testing.T) {
	a, iw, _ := instanceAgent(t, models.MODE_TEST)
	p := &lossWire{instanceWire: iw}
	a.Proxy = p
	ctx := context.Background()
	a.proven.losses = a.markCaptureLosses(ctx)
	a.proven.pool = map[string][]string{"a": {"pa"}, "b": {"pb"}}
	w1, w2 := child(t), child(t)
	_, serr := claimAsWorker(a, w1.Process.Pid, "a")
	require.Nil(t, serr)
	_, serr = claimAsWorker(a, w2.Process.Pid, "b")
	require.Nil(t, serr)
	run := func(pid int, name string, during func()) {
		t.Helper()
		_, serr := a.ProvenBegin(as(pid), begin(name))
		require.Nil(t, serr)
		during()
		_, serr = a.ProvenEnd(as(pid), models.ScopeReq{Name: name, Outcome: models.ScopeOutcomePassed})
		require.Nil(t, serr)
	}
	// The windows so far, not the run's final read, which ends the run.
	windowNow := func(id string) models.ScopeWindow {
		t.Helper()
		for _, w := range a.scopeWindowsNow() {
			if w.Name == id {
				return w
			}
		}
		t.Fatalf("no window of %s", id)
		return models.ScopeWindow{}
	}
	charged := func(id string) models.CaptureLosses {
		t.Helper()
		if w := windowNow(id); w.Evidence != nil {
			return w.Evidence.CannotCapture
		}
		return nil
	}
	refused := func(pid uint64) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossUnattributed, Count: 1, What: fmt.Sprintf("the latest: a connection to 10.0.0.9:5432, from process %d, in no test worker's keploy instance", pid)}}
	}

	run(w1.Process.Pid, "TestOne", func() { p.refuse(1) })
	require.Equal(t, refused(4001), charged("a::TestOne"), "worker 1's test was open as the call was refused")
	w := windowNow("a::TestOne")
	require.Equal(t, models.ScopeOutcomePassed, w.Outcome, "a refused call leaves the test without a verdict, never failed")
	require.Contains(t, w.Evidence.Reason, "a replay refuses them unanswered")

	run(w2.Process.Pid, "TestTwo", func() {})
	require.Empty(t, charged("b::TestTwo"), "a test begun after the refusal was charged is charged it again")

	p.refuse(2)
	run(w1.Process.Pid, "TestThree", func() {})
	require.Equal(t, refused(4002), charged("a::TestThree"), "a call refused while no test was open is the next test's")
}

// A replay that learns nothing takes each worker's claim, each served from a
// manager of its own: only record's full round refuses them
// (learnWorkersRefusal).
func TestAReplayTakesEachWorkersClaim(t *testing.T) {
	a, p, h := instanceAgent(t, models.MODE_TEST)
	w1, w2 := child(t), child(t)
	require.Equal(t, "1", claimWorker(t, a, w1.Process.Pid, "a", 2))
	require.Equal(t, "2", claimWorker(t, a, w2.Process.Pid, "b", 2))
	require.Equal(t, map[uint32]uint32{1: uint32(w1.Process.Pid), 2: uint32(w2.Process.Pid)}, h.given())
	served, _ := p.log()
	require.Equal(t, []uint32{1, 2}, served)
}

// In a replay, different workers' tests are open at once, each served on
// its own worker's manager and charged its own worker's misses alone,
// whichever ends first.
func TestWorkersTestsOverlapAtReplay(t *testing.T) {
	a, p, _ := instanceAgent(t, models.MODE_TEST)
	a.proven.pool = map[string][]string{"a": {"pa"}, "b": {"pb"}}
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr, "another worker's test may be open")
	one, two := p.worker(t, 1), p.worker(t, 2)
	require.Equal(t, "a::TestOne", one.exactTest)
	require.Equal(t, "b::TestTwo", two.exactTest)
	require.Empty(t, p.lastTest, "the run's manager serves no worker's test")
	one.mu.Lock()
	one.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /one", Instance: 1}}
	one.mu.Unlock()
	two.mu.Lock()
	two.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /two", Instance: 2}}
	two.mu.Unlock()

	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Equal(t, "a::TestOne", a.proven.openOf(1), "its end leaves the other worker's test open")
	require.Equal(t, "a::TestOne", one.exactTest, "its end leaves the other worker's test served")
	require.Equal(t, []string{"pb"}, two.exactNames, "its end serves its own worker its key's pool")
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	for id, want := range map[string]string{"a::TestOne": "GET /one", "b::TestTwo": "GET /two"} {
		w := windowOf(t, a, id)
		require.Equal(t, models.ScopeOutcomeFailed, w.Outcome, id)
		require.Len(t, w.Evidence.Misses, 1, id)
		require.Equal(t, want, w.Evidence.Misses[0].ActualSummary, id)
	}
	require.EqualValues(t, 1, windowOf(t, a, "a::TestOne").Instance)
	require.EqualValues(t, 2, windowOf(t, a, "b::TestTwo").Instance)
}

// Each test worker's test keeps its own browser seed, whatever other
// worker's test is open beside it: the seed is the test's, never the
// worker's or the last begin's. At record each begin is answered the seed it
// sent, an end's word on how the browser drew replaces only its own test's,
// and a retry of a test draws its first attempt's. At replay each begin is
// answered its own test's recorded seed. Each window carries its seed beside
// its worker's instance.
func TestEachWorkersTestKeepsItsOwnSeedWhileAnothersIsOpen(t *testing.T) {
	ctx := context.Background()
	seeds := func(t *testing.T, a *Agent) {
		t.Helper()
		one, two := windowOf(t, a, "a::TestOne"), windowOf(t, a, "b::TestTwo")
		require.Equal(t, "1111222233334444", one.Seed)
		require.Equal(t, "aaaabbbbccccdddd", two.Seed)
		require.Equal(t, models.ScopeRandomOwn, one.Random, "its end's word")
		require.Equal(t, models.ScopeRandomSeeded, two.Random, "the other end's word is not its")
		require.EqualValues(t, 1, one.Instance)
		require.EqualValues(t, 2, two.Instance)
		require.Equal(t, workerDir("a"), one.Dir, "its worker's folder")
		require.Equal(t, workerDir("b"), two.Dir, "its worker's folder")
	}
	end := func(t *testing.T, a *Agent, w1, w2 int) {
		t.Helper()
		_, serr := a.ProvenEnd(as(w1), models.ScopeReq{Name: "TestOne", Outcome: "passed", Random: models.ScopeRandomOwn})
		require.Nil(t, serr)
		_, serr = a.ProvenEnd(as(w2), models.ScopeReq{Name: "TestTwo", Outcome: "passed"})
		require.Nil(t, serr)
	}

	t.Run("record", func(t *testing.T) {
		a, _ := workerAgent(t, models.MODE_RECORD, 4)
		w1, w2 := child(t), child(t)
		claimWorker(t, a, w1.Process.Pid, "a", 2)
		claimWorker(t, a, w2.Process.Pid, "b", 2)
		one, serr := a.ProvenBegin(as(w1.Process.Pid), seededBegin("TestOne", "1111222233334444"))
		require.Nil(t, serr)
		two, serr := a.ProvenBegin(as(w2.Process.Pid), seededBegin("TestTwo", "AAAABBBBCCCCDDDD"))
		require.Nil(t, serr, "another worker's test may be open")
		require.Equal(t, "1111222233334444", one.Seed, "the seed it sent")
		require.Equal(t, "aaaabbbbccccdddd", two.Seed, "the seed it sent")
		end(t, a, w1.Process.Pid, w2.Process.Pid)
		// A retry of the other worker's test, begun while this worker's
		// next test is open, draws its own first attempt's numbers.
		_, serr = a.ProvenBegin(as(w1.Process.Pid), seededBegin("TestThree", "5555666677778888"))
		require.Nil(t, serr)
		retry, serr := a.ProvenBegin(as(w2.Process.Pid), seededBegin("TestTwo", "9999000099990000"))
		require.Nil(t, serr)
		require.Equal(t, "aaaabbbbccccdddd", retry.Seed, "a retry draws its first attempt's numbers")
		for _, w := range []struct {
			pid  int
			name string
		}{{w2.Process.Pid, "TestTwo"}, {w1.Process.Pid, "TestThree"}} {
			_, serr = a.ProvenEnd(as(w.pid), models.ScopeReq{Name: w.name, Outcome: "passed"})
			require.Nil(t, serr)
		}
		seeds(t, a)
		ws, err := a.FinalScopeWindows(ctx)
		require.NoError(t, err)
		kept := map[string][]string{}
		for _, w := range ws {
			if w.Instance != 0 {
				kept[w.Name] = append(kept[w.Name], w.Seed)
			}
		}
		require.Equal(t, map[string][]string{
			"a::TestOne":   {"1111222233334444"},
			"b::TestTwo":   {"aaaabbbbccccdddd", "aaaabbbbccccdddd"},
			"a::TestThree": {"5555666677778888"},
		}, kept)
	})

	t.Run("replay", func(t *testing.T) {
		a, _, _ := instanceAgent(t, models.MODE_TEST)
		a.proven.pool = map[string][]string{"a": {"pa"}, "b": {"pb"}}
		a.SetTestSeeds(ctx, map[string]string{"a::TestOne": "1111222233334444", "b::TestTwo": "aaaabbbbccccdddd"})
		w1, w2 := child(t), child(t)
		claimWorker(t, a, w1.Process.Pid, "a", 2)
		claimWorker(t, a, w2.Process.Pid, "b", 2)
		one, serr := a.ProvenBegin(as(w1.Process.Pid), seededBegin("TestOne", "fedcba9876543210"))
		require.Nil(t, serr)
		two, serr := a.ProvenBegin(as(w2.Process.Pid), seededBegin("TestTwo", "fedcba9876543210"))
		require.Nil(t, serr, "another worker's test may be open")
		require.Equal(t, "1111222233334444", one.Seed, "its own recording's")
		require.Equal(t, "aaaabbbbccccdddd", two.Seed, "its own recording's")
		end(t, a, w1.Process.Pid, w2.Process.Pid)
		seeds(t, a)
	})
}

// A test worker's claim names its folder, as keploy's Go SDK sends it: the
// begin notes the folder first, which starts the test's notes afresh, then
// its seed and its worker. So each window keeps its folder, its own seed
// and its worker's instance; a begin that noted the worker before the
// folder would leave every window with no worker's instance, and charge
// each worker's test with the calls of none.
func TestAWorkersClaimWithItsFolderKeepsItsInstanceAndSeedOnItsWindows(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	w1, w2 := child(t), child(t)
	for _, c := range []struct {
		pid      int
		key, dir string
	}{{w1.Process.Pid, "a", "/work/pkg/a"}, {w2.Process.Pid, "b", "/work/pkg/b"}} {
		_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: c.key, Dir: c.dir, Workers: 2}, c.pid)
		require.Nil(t, serr)
	}
	_, serr := a.ProvenBegin(as(w1.Process.Pid), seededBegin("TestOne", "1111222233334444"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), seededBegin("TestTwo", "aaaabbbbccccdddd"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: "passed"})
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: "passed"})
	require.Nil(t, serr)
	one, two := windowOf(t, a, "a::TestOne"), windowOf(t, a, "b::TestTwo")
	require.Equal(t, "/work/pkg/a", one.Dir)
	require.Equal(t, "/work/pkg/b", two.Dir)
	require.EqualValues(t, 1, one.Instance)
	require.EqualValues(t, 2, two.Instance)
	require.Equal(t, "1111222233334444", one.Seed)
	require.Equal(t, "aaaabbbbccccdddd", two.Seed)
}

// A replay whose build moves the run's one clock to each test's recorded
// begin takes one test worker: a claim of two is refused at the claim,
// named, before any test runs, so whether the run is taken never depends on
// how its workers' tests happen to overlap. A claim of one is taken, and its
// test moves the clock. A record moves no clock, and takes both workers,
// whose tests overlap.
func TestAClockMovingReplayRefusesMoreThanOneWorkerAtTheClaim(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_TEST, models.MODE_RECORD} {
		t.Run(string(mode), func(t *testing.T) {
			a, p, h := instanceAgent(t, mode)
			clock := &clockHooks{}
			useHooks(t, clock)
			w1, w2 := child(t), child(t)
			if mode == models.MODE_RECORD {
				claimWorker(t, a, w1.Process.Pid, "a", 2)
				claimWorker(t, a, w2.Process.Pid, "b", 2)
				_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
				require.Nil(t, serr)
				_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
				require.Nil(t, serr, "a record moves no clock")
				require.Equal(t, "b::TestTwo", a.proven.openOf(2))
				require.Empty(t, clock.moves)
				return
			}
			_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a", Workers: 2}, w1.Process.Pid)
			require.NotNil(t, serr, "a replay on the run's one clock takes no second worker")
			require.Equal(t, models.ScopeErrTooManyWorkers, serr.Code)
			require.Contains(t, serr.Message, "a test process declared 2 test workers, but this replay moves the run's clock to each test's recorded begin, and a run has one clock")
			require.Contains(t, serr.Remedy, "replay with one worker")
			refusedAlike(t, a, models.InstanceReq{Key: "b", Workers: 2}, w2.Process.Pid, serr, w1.Process.Pid)
			require.Empty(t, h.given(), "the refused claim was given no instance")
			served, _ := p.log()
			require.Empty(t, served, "the refused claim was served no manager")
			require.Empty(t, a.proven.workers)

			claimWorker(t, a, w1.Process.Pid, "a", 1)
			_, serr = a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
			require.Nil(t, serr, "one worker runs on the run's one clock")
			require.Len(t, clock.moves, 1)
			require.Contains(t, clock.moves[0], "begin a::TestOne")
		})
	}
}

// A replay whose session reads each set's end takes one test worker, in
// every build, whatever MaxWorkers says: a claim of two is refused at the
// claim, named, before any test runs, since a call a worker's own store
// holds for a later test would be filed only when its connection ends,
// after the set's last read. So is a claim before any session said it reads
// none. A replay whose session reads none takes both, and so does a record,
// which files nothing at a set's end.
func TestAReplayThatReadsSetEndsRefusesMoreThanOneWorkerAtTheClaim(t *testing.T) {
	for _, c := range []struct {
		name        string
		mode        models.Mode
		reads, said bool
		refused     bool
	}{
		{"replay reads set ends", models.MODE_TEST, true, true, true},
		{"replay before its session's word", models.MODE_TEST, false, false, true},
		{"replay reads none", models.MODE_TEST, false, true, false},
		{"record reads set ends", models.MODE_RECORD, true, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, p, h := instanceAgent(t, c.mode)
			if c.said {
				a.proven.readsNoSetEnd = !c.reads
			} else {
				// A new session, which has not said yet.
				require.NoError(t, a.resetSession(context.Background()))
			}
			w1, w2 := child(t), child(t)
			if !c.refused {
				claimWorker(t, a, w1.Process.Pid, "a", 2)
				claimWorker(t, a, w2.Process.Pid, "b", 2)
				require.Len(t, a.proven.workers, 2)
				return
			}
			_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a", Workers: 2}, w1.Process.Pid)
			require.NotNil(t, serr, "a worker's own store files nothing at the set's end")
			require.Equal(t, models.ScopeErrTooManyWorkers, serr.Code)
			require.Equal(t, http.StatusUnprocessableEntity, serr.Status)
			require.Contains(t, serr.Message, "a test process declared 2 test workers, but this replay reads each test set's end, and a call a test worker's own store holds for a later test would be reported only when its connection ends, after that read")
			require.Equal(t, "replay with one worker (go test -p 1, Playwright workers: 1, pytest without -n, jest --runInBand)", serr.Remedy)
			refusedAlike(t, a, models.InstanceReq{Key: "b", Workers: 2}, w2.Process.Pid, serr, w1.Process.Pid)
			require.Empty(t, h.given(), "the refused claim was given no instance")
			served, _ := p.log()
			require.Empty(t, served, "the refused claim was served no manager")
			require.Empty(t, a.proven.workers)

			claimWorker(t, a, w1.Process.Pid, "a", 1)
			require.Equal(t, w1.Process.Pid, a.proven.claim.pid, "one test process runs the replay")
			require.Empty(t, a.proven.workers)
		})
	}
}

// moverClock is a build's clock that BeginTestClock moves in this run only
// when here says so (TestClockMover).
type moverClock struct {
	clockHooks
	here bool
}

func (h *moverClock) MovesTestClock() bool { return h.here }

// Whether a replay takes more than one test worker is what the build's
// BeginTestClock does in this run (TestClockMover), not that the build has
// a TestClock: one that moves the run's clock here refuses a claim of two
// workers, named, and one that moves no clock here (the enterprise build
// without a clock record of the run's own) takes both and runs their tests
// at once, each to its own end, and moves no clock for either.
func TestAReplayTakesWorkersOnlyWhereTheBuildMovesNoClockInThisRun(t *testing.T) {
	for _, here := range []bool{true, false} {
		t.Run(fmt.Sprintf("moves=%v", here), func(t *testing.T) {
			a, _, h := instanceAgent(t, models.MODE_TEST)
			clock := &moverClock{here: here}
			useHooks(t, clock)
			w1, w2 := child(t), child(t)
			if here {
				_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a", Workers: 2}, w1.Process.Pid)
				require.NotNil(t, serr)
				require.Equal(t, models.ScopeErrTooManyWorkers, serr.Code)
				require.Contains(t, serr.Message, "this replay moves the run's clock to each test's recorded begin")
				require.Empty(t, h.given())
				require.Empty(t, clock.moves)
				return
			}
			claimWorker(t, a, w1.Process.Pid, "a", 2)
			claimWorker(t, a, w2.Process.Pid, "b", 2)
			_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
			require.Nil(t, serr)
			_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
			require.Nil(t, serr, "a build that moves no clock in this run holds no test on it")
			require.Equal(t, "a::TestOne", a.proven.openOf(1))
			require.Equal(t, "b::TestTwo", a.proven.openOf(2))
			_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
			require.Nil(t, serr)
			_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
			require.Nil(t, serr)
			require.Empty(t, a.proven.openOf(1))
			require.Empty(t, a.proven.openOf(2))
			require.Empty(t, clock.moves, "no clock is moved in this run")
		})
	}
}

// A call a replay of test workers refused while two workers' tests were
// open is charged to both, and leaves each without a verdict.
func TestAReplaysRefusedCallDuringAnOverlapIsChargedToEveryOpenTest(t *testing.T) {
	a, iw, _ := instanceAgent(t, models.MODE_TEST)
	p := &lossWire{instanceWire: iw}
	a.Proxy = p
	a.proven.losses = a.markCaptureLosses(context.Background())
	a.proven.pool = map[string][]string{"a": {"pa"}, "b": {"pb"}}
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	p.refuse(1)
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	want := models.CaptureLosses{{Kind: models.CaptureLossUnattributed, Count: 1, What: "the latest: a connection to 10.0.0.9:5432, from process 4001, in no test worker's keploy instance"}}
	for _, id := range []string{"a::TestOne", "b::TestTwo"} {
		w := windowOf(t, a, id)
		require.NotNil(t, w.Evidence, id)
		require.Equal(t, want, w.Evidence.CannotCapture, id)
		require.Contains(t, w.Evidence.Reason, "a replay refuses them unanswered", id)
	}
}
