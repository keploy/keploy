//go:build linux

package agent

import (
	"context"
	"errors"
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
// read while the test was still served.
type workerWire struct {
	*wireProxy
	readOpen bool
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
// under the wire, as ClaimInstance does past its refusal of a replay's
// workers (replayWorkersRefusal).
func claimAsWorker(a *Agent, pid int, key string) (models.InstanceResp, *models.ScopeError) {
	ctx := context.Background()
	if err := a.wire.lock(ctx); err != nil {
		return models.InstanceResp{}, abandoned(err)
	}
	defer a.wire.unlock()
	start, _ := procStart(pid)
	return a.claimWorker(ctx, models.InstanceReq{Key: key, Workers: 2}, claimant{pid: pid, start: start}, 2)
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

// The CLI's pool slices reach each replay worker none of whose tests is
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
