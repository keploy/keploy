//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// instanceHooks are hooks that put processes in keploy instances
// (coreAgent.InstanceStamper), keeping what they were asked.
type instanceHooks struct {
	countHooks
	imu  sync.Mutex
	in   map[uint32]uint32 // id -> the tgid given it
	held map[uint32]bool   // ids a gone worker's survivor is still in
	// allHeld: every id is still held.
	allHeld bool
	tried   []uint32 // every id SetInstance was asked for
	ended   []uint32
	err     error
	endErr  error
}

func (h *instanceHooks) SetInstance(tgid, id uint32) error {
	h.imu.Lock()
	defer h.imu.Unlock()
	h.tried = append(h.tried, id)
	if h.err != nil {
		return h.err
	}
	if h.allHeld || h.held[id] {
		return fmt.Errorf("%w: instance %d", coreAgent.ErrInstanceHeld, id)
	}
	if _, ok := h.in[id]; ok {
		return fmt.Errorf("%w: instance %d given twice", coreAgent.ErrInstanceHeld, id)
	}
	if h.in == nil {
		h.in = map[uint32]uint32{}
	}
	h.in[id] = tgid
	return nil
}

func (h *instanceHooks) EndInstance(id uint32) (int, error) {
	h.imu.Lock()
	defer h.imu.Unlock()
	h.ended = append(h.ended, id)
	if h.endErr != nil {
		return 0, h.endErr
	}
	_, ok := h.in[id]
	delete(h.in, id)
	if ok {
		return 1, nil
	}
	return 0, nil
}

func (h *instanceHooks) InstanceLost(uint32) (coreAgent.InstanceLoss, error) {
	return coreAgent.InstanceLoss{}, nil
}

func (h *instanceHooks) given() map[uint32]uint32 {
	h.imu.Lock()
	defer h.imu.Unlock()
	out := map[uint32]uint32{}
	for id, tgid := range h.in {
		out[id] = tgid
	}
	return out
}

func (h *instanceHooks) triedIDs() []uint32 {
	h.imu.Lock()
	defer h.imu.Unlock()
	return append([]uint32(nil), h.tried...)
}

func (h *instanceHooks) endedIDs() []uint32 {
	h.imu.Lock()
	defer h.imu.Unlock()
	return append([]uint32(nil), h.ended...)
}

// workerAgent is a wire agent whose hooks give instances, in a build that
// runs up to most workers at once.
func workerAgent(t *testing.T, mode models.Mode, most int) (*Agent, *instanceHooks) {
	t.Helper()
	a, _ := wireAgent(t, mode)
	h := &instanceHooks{}
	a.Hooks = h
	setMaxWorkers(t, most)
	return a, h
}

func setMaxWorkers(t *testing.T, most int) {
	t.Helper()
	was := MaxWorkers
	MaxWorkers = func() int { return most }
	t.Cleanup(func() { MaxWorkers = was })
}

// as is a context whose begin or end comes from process pid.
func as(pid int) context.Context {
	return coreAgent.WithCaller(context.Background(), func() (int, error) { return pid, nil })
}

func claimWorker(t *testing.T, a *Agent, pid int, key string, workers int) string {
	t.Helper()
	resp, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: key, Workers: workers}, pid)
	require.Nil(t, serr)
	return resp.Instance
}

// A claim that declares more workers than this keploy runs is refused at
// once, with nothing else running: the run's outcome does not depend on
// whether its workers would have overlapped. Every test process of the run
// that claims is refused in the same words, naming none of them, so the
// run's refusal is said once. An integration that declares none, or one,
// runs as before.
func TestAClaimOfMoreWorkersThanKeployRunsIsRefusedAtOnce(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "o", Workers: 4}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrTooManyWorkers, serr.Code)
	require.Equal(t, http.StatusUnprocessableEntity, serr.Status)
	require.Equal(t, "a test process declared 4 test workers, but this keploy runs one test worker at a time", serr.Message)
	require.Contains(t, serr.Remedy, "Playwright workers: 1")
	refusedAlike(t, a, models.InstanceReq{Key: "o", Workers: 4}, child(t).Process.Pid, serr, os.Getpid())

	for _, n := range []int{0, 1} {
		resp, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "o", Workers: n}, os.Getpid())
		require.Nil(t, serr, "workers %d", n)
		require.Equal(t, models.InstanceRun, resp.Instance)
	}

	setMaxWorkers(t, 3)
	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "o", Workers: 4}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrTooManyWorkers, serr.Code)
	require.Contains(t, serr.Message, "runs at most 3 test workers at once")
	require.Equal(t, "run at most 3 test workers", serr.Remedy)
	// As many as the limit is not too many (these hooks then cannot stamp
	// the workers, which is another refusal).
	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "o", Workers: 3}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInInstance, serr.Code)
}

// A second test process that claimed the run while the first still runs is
// told to declare the run's test workers only where this run would take
// them: a build that runs them, with nothing of the run refusing them.
func TestASecondProcesssFixNamesWorkersOnlyWhereTheRunTakesThem(t *testing.T) {
	const one = "run one test process at a time (go test -p 1, pytest without -n, jest --runInBand)"
	for _, c := range []struct {
		name string
		set  func(*testing.T, *Agent, *instanceHooks)
		fix  string
	}{
		{"taken", func(*testing.T, *Agent, *instanceHooks) {}, one + ", or declare, in every test process's claim, how many test workers the run starts at once"},
		{"no capture-loss count", func(_ *testing.T, _ *Agent, h *instanceHooks) {
			h.countingHooks.err = models.ErrPIDMapRefusalsUnsupported
		}, one},
		{"record's full round", func(_ *testing.T, a *Agent, _ *instanceHooks) { a.proven.order.learn = true }, one},
		{"one worker", func(t *testing.T, _ *Agent, _ *instanceHooks) { setMaxWorkers(t, 1) }, one},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, h := workerAgent(t, models.MODE_RECORD, 4)
			c.set(t, a, h)
			first := child(t)
			_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a"}, first.Process.Pid)
			require.Nil(t, serr)
			_, serr = a.ClaimInstance(context.Background(), models.InstanceReq{Key: "b"}, os.Getpid())
			require.NotNil(t, serr)
			require.Equal(t, models.ScopeErrOneTestProcess, serr.Code)
			require.Equal(t, c.fix, serr.Remedy)
		})
	}
}

func TestANegativeWorkerCountIsABadRequest(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "o", Workers: -1}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrBadRequest, serr.Code)
	require.Equal(t, http.StatusBadRequest, serr.Status)
}

// Each worker of a run of more than one gets a keploy instance of its own,
// its process put in it by its TGID; one more than the run declared is
// refused, naming the workers that run.
func TestEachWorkerOfARunGetsAnInstanceOfItsOwn(t *testing.T) {
	a, h := workerAgent(t, models.MODE_RECORD, 4)
	w1, w2 := child(t), child(t)
	require.Equal(t, "1", claimWorker(t, a, w1.Process.Pid, "a", 2))
	require.Equal(t, "2", claimWorker(t, a, w2.Process.Pid, "b", 2))
	require.Equal(t, map[uint32]uint32{1: uint32(w1.Process.Pid), 2: uint32(w2.Process.Pid)}, h.given())
	require.Equal(t, []uint32{1, 2}, h.triedIDs(), "a live worker's id is not asked for")

	_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "c", Workers: 2}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrOneTestProcess, serr.Code)
	require.Contains(t, serr.Message, fmt.Sprintf("2 test workers, as many as the run declared, still run them (processes %d, %d)", w1.Process.Pid, w2.Process.Pid))

	_, serr = a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a", Workers: 2}, w1.Process.Pid)
	require.NotNil(t, serr, "a worker claims once")
	require.Equal(t, models.ScopeErrOneTestProcess, serr.Code)
	require.Contains(t, serr.Message, "already test worker 1")

	ws, err := a.FinalScopeWindows(context.Background())
	require.NoError(t, err)
	var claims []string
	for _, w := range ws {
		if w.Claim != "" {
			claims = append(claims, w.Claim+"@"+strconv.Itoa(int(w.Worker)))
		}
	}
	require.Equal(t, []string{"a@1", "b@2"}, claims)
}

// Every worker live at once declares the same count; a single test process
// and a worker never run at once either.
func TestAWorkerCountOtherThanALiveWorkersIsRefused(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	ctx := context.Background()
	w1 := child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "b", Workers: 3}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrWorkersMismatch, serr.Code)
	require.Contains(t, serr.Message, fmt.Sprintf("declared 3 test workers, but test worker 1 (process %d) declared 2", w1.Process.Pid))
	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "b"}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrWorkersMismatch, serr.Code)
	require.Contains(t, serr.Message, fmt.Sprintf("declared one test worker while test worker 1 (process %d), which declared 2", w1.Process.Pid))

	b, _ := workerAgent(t, models.MODE_RECORD, 4)
	single := child(t)
	claimWorker(t, b, single.Process.Pid, "s", 0)
	_, serr = b.ClaimInstance(ctx, models.InstanceReq{Key: "b", Workers: 2}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrWorkersMismatch, serr.Code)
	require.Contains(t, serr.Message, fmt.Sprintf("while process %d, which declared one, still runs", single.Process.Pid))
}

// A worker that exited: its open test fails, its instance ends, and a new
// worker can take its place (and its id).
func TestAGoneWorkersInstanceEndsAndItsOpenTestFails(t *testing.T) {
	a, h := workerAgent(t, models.MODE_RECORD, 4)
	gone, stays := child(t), child(t)
	claimWorker(t, a, gone.Process.Pid, "a", 2)
	claimWorker(t, a, stays.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(gone.Process.Pid), begin("TestCrash"))
	require.Nil(t, serr)
	kill(t, gone)

	require.Equal(t, "1", claimWorker(t, a, os.Getpid(), "c", 2), "the gone worker's id is free again")
	require.Equal(t, []uint32{1}, h.endedIDs())
	w := windowOf(t, a, "a::TestCrash")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Contains(t, w.Evidence.Reason, "the test worker's process exited while the test was open")
}

// A gone worker's end leaves another worker's open test open.
func TestAGoneWorkerLeavesAnotherWorkersOpenTestOpen(t *testing.T) {
	a, h := workerAgent(t, models.MODE_RECORD, 4)
	gone, stays := child(t), child(t)
	claimWorker(t, a, gone.Process.Pid, "a", 2)
	claimWorker(t, a, stays.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(stays.Process.Pid), begin("TestKeep"))
	require.Nil(t, serr)
	kill(t, gone)
	require.Equal(t, "1", claimWorker(t, a, os.Getpid(), "c", 2))
	require.Equal(t, []uint32{1}, h.endedIDs())
	_, serr = a.ProvenEnd(as(stays.Process.Pid), models.ScopeReq{Name: "TestKeep", Outcome: "passed"})
	require.Nil(t, serr, "the live worker's test is still open")
	require.Equal(t, models.ScopeOutcomePassed, windowOf(t, a, "b::TestKeep").Outcome)
}

// A begin is its worker's only when both its process ID and its start time
// are the worker's: a process on a gone worker's reused pid, or another
// process started in the same clock tick, is no worker.
func TestACallersPidAndStartMustBothBeTheWorkers(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	w1 := child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	me, ok := procStart(os.Getpid())
	require.True(t, ok)
	a.provenMu.Lock()
	w := a.proven.workers[1]
	w.start = me // another process, w1, started in the tick this one did
	a.proven.workers[1] = w
	a.provenMu.Unlock()
	_, serr := a.ProvenBegin(as(os.Getpid()), begin("TestX"))
	require.NotNil(t, serr, "this process has the worker's start, not its pid")
	require.Equal(t, models.ScopeErrNotInInstance, serr.Code)

	a.provenMu.Lock()
	w.pid, w.start = os.Getpid(), me+1 // a gone worker whose pid this process reuses
	a.proven.workers[1] = w
	a.provenMu.Unlock()
	_, serr = a.ProvenBegin(as(os.Getpid()), begin("TestX"))
	require.NotNil(t, serr, "this process has the worker's pid, not its start")
	require.Equal(t, models.ScopeErrNotInInstance, serr.Code)
}

// A gone single test process's open test fails when a worker claims, and
// the worker then runs.
func TestAWorkerClaimFailsAGoneTestProcesssOpenTest(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	gone := child(t)
	claimWorker(t, a, gone.Process.Pid, "s", 1)
	_, serr := a.ProvenBegin(as(gone.Process.Pid), begin("TestCrash"))
	require.Nil(t, serr)
	kill(t, gone)
	require.Equal(t, "1", claimWorker(t, a, os.Getpid(), "a", 2))
	_, serr = a.ProvenBegin(as(os.Getpid()), begin("TestNext"))
	require.Nil(t, serr, "the gone process's test was closed at the claim")
	w := windowOf(t, a, "s::TestCrash")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Contains(t, w.Evidence.Reason, "the test process exited while the test was open")
}

// In a run of workers, a test is the worker's whose process sent its begin:
// named by that worker's key. A process that is no worker is refused, and
// so is a begin whose caller keploy cannot read.
func TestAWorkersTestIsTheWorkersWhoseProcessBeganIt(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)

	_, serr := a.ProvenBegin(as(os.Getpid()), begin("TestX"))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInInstance, serr.Code)
	require.Contains(t, serr.Message, fmt.Sprintf("process %d sent TestX's begin, but it is not a live test worker of this run", os.Getpid()))
	_, serr = a.ProvenBegin(context.Background(), begin("TestX"))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNoCaller, serr.Code)
	_, serr = a.ProvenBegin(coreAgent.WithCaller(context.Background(), func() (int, error) { return 0, errors.New("no socket") }), begin("TestX"))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNoCaller, serr.Code)
	require.Contains(t, serr.Message, "no socket")

	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestX"))
	require.Nil(t, serr)
	// Another worker's end touches nothing of worker 2's.
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestX", Outcome: "passed"})
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotOpen, serr.Code)
	require.Contains(t, serr.Message, "a::TestX ended, but no test is open")
	_, serr = a.ProvenEnd(as(os.Getpid()), models.ScopeReq{Name: "TestX", Outcome: "passed"})
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInInstance, serr.Code)

	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestX", Outcome: "passed"})
	require.Nil(t, serr, "worker 2's own test is still open, and it ends it")
	_, serr = a.ProvenBegin(as(w1.Process.Pid), begin("TestX"))
	require.Nil(t, serr)
	require.Equal(t, "a::TestX", a.proven.openOf(1))
	require.Empty(t, a.proven.openOf(2))
	require.Equal(t, models.ScopeOutcomePassed, windowOf(t, a, "b::TestX").Outcome)
}

// Record's full round refuses a run of more than one worker at its first
// claim, named, and every other test process's claim in the same words: its
// learn step keeps the set's tests to their recorded order, which tests run
// at once in several workers do not keep. The fix
// names only what works: a replay may move the run's one clock, and then
// takes no workers either (clockWorkersRefusal).
func TestAWorkerRunIsRefusedInRecordsFullRound(t *testing.T) {
	a, h := workerAgent(t, models.MODE_TEST, 4)
	a.proven.order.learn = true
	_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a", Workers: 2}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrTooManyWorkers, serr.Code)
	require.Contains(t, serr.Message, "a test process declared 2 test workers, but record's full round replays one test worker at a time")
	require.Equal(t, "record with one worker (go test -p 1, Playwright workers: 1, pytest without -n, jest --runInBand)", serr.Remedy)
	refusedAlike(t, a, models.InstanceReq{Key: "b", Workers: 2}, child(t).Process.Pid, serr, os.Getpid())
	require.Empty(t, h.given())
	require.Empty(t, a.proven.workers)
}

// Hooks that cannot stamp a worker on its connections, or that refuse its
// process, refuse its claim.
func TestAWorkerKeployCannotGiveAnInstanceIsRefused(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	setMaxWorkers(t, 4)
	_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a", Workers: 2}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInInstance, serr.Code)
	require.Equal(t, "a test process declared 2 test workers, but keploy's hooks cannot tell one worker's calls from another's", serr.Message)
	refusedAlike(t, a, models.InstanceReq{Key: "b", Workers: 2}, child(t).Process.Pid, serr, os.Getpid())

	b, h := workerAgent(t, models.MODE_RECORD, 4)
	h.err = errors.New("keploy's agent runs in a PID namespace of its own")
	_, serr = b.ClaimInstance(context.Background(), models.InstanceReq{Key: "a", Workers: 2}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInInstance, serr.Code)
	require.Contains(t, serr.Message, "PID namespace of its own")
	require.Empty(t, b.proven.workers, "a refused claim is no worker")
}

// An id a gone worker's survivor is still in is not given: the next is.
// With every id held, the claim is refused.
func TestAnInstanceStillHeldIsSkipped(t *testing.T) {
	a, h := workerAgent(t, models.MODE_RECORD, 4)
	h.held = map[uint32]bool{1: true, 2: true}
	require.Equal(t, "3", claimWorker(t, a, os.Getpid(), "a", 2))

	b, hb := workerAgent(t, models.MODE_RECORD, 4)
	hb.allHeld = true
	_, serr := b.ClaimInstance(context.Background(), models.InstanceReq{Key: "a", Workers: 2}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInInstance, serr.Code)
	require.Contains(t, serr.Message, "still in every one of its 1023 instances")
	require.Len(t, hb.triedIDs(), int(coreAgent.MaxInstanceID))
	require.Empty(t, b.proven.workers)
}

// An instance the kernel could not end is said in the agent's log; one it
// ended is not.
func TestAnInstanceTheKernelCouldNotEndIsLogged(t *testing.T) {
	a, h := workerAgent(t, models.MODE_RECORD, 4)
	core, logs := observer.New(zap.WarnLevel)
	a.logger = zap.New(core)
	claimWorker(t, a, os.Getpid(), "a", 2)
	require.NoError(t, a.resetSession(context.Background()))
	require.Zero(t, logs.Len())
	claimWorker(t, a, os.Getpid(), "a", 2)
	h.endErr = errors.New("map busy")
	require.NoError(t, a.resetSession(context.Background()))
	require.Equal(t, 1, logs.Len())
	require.Contains(t, logs.All()[0].Message, "could not end a test worker's instance")
}

// A new session, and the run's final read, end every worker's instance.
func TestANewSessionAndTheFinalReadEndTheWorkersInstances(t *testing.T) {
	a, h := workerAgent(t, models.MODE_RECORD, 4)
	ctx := context.Background()
	w1 := child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, os.Getpid(), "b", 2)
	require.NoError(t, a.resetSession(ctx))
	require.Equal(t, []uint32{1, 2}, h.endedIDs())
	require.Empty(t, a.proven.workers)

	require.Equal(t, "1", claimWorker(t, a, w1.Process.Pid, "a", 2))
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOpen"))
	require.Nil(t, serr)
	_, err := a.FinalScopeWindows(ctx)
	require.NoError(t, err)
	require.Equal(t, []uint32{1, 2, 1}, h.endedIDs())
	w := windowOf(t, a, "a::TestOpen")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Contains(t, w.Evidence.Reason, fmt.Sprintf("its process %d outlived the test command", w1.Process.Pid))
	require.Equal(t, []uint32{1, 2, 1}, h.endedIDs(), "a second read ends nothing again")
}

// Each worker's test window carries that worker's keploy instance, by which
// record charges it only that worker's calls.
func TestAWorkersTestWindowCarriesItsInstance(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	w1, w2 := child(t), child(t)
	require.Equal(t, "1", claimWorker(t, a, w1.Process.Pid, "a", 2))
	require.Equal(t, "2", claimWorker(t, a, w2.Process.Pid, "b", 2))
	for _, w := range []struct {
		pid  int
		name string
	}{{w2.Process.Pid, "TestTwo"}, {w1.Process.Pid, "TestOne"}} {
		_, serr := a.ProvenBegin(as(w.pid), begin(w.name))
		require.Nil(t, serr)
		_, serr = a.ProvenEnd(as(w.pid), models.ScopeReq{Name: w.name, Outcome: "passed"})
		require.Nil(t, serr)
	}
	require.EqualValues(t, 1, windowOf(t, a, "a::TestOne").Instance)
	require.EqualValues(t, 2, windowOf(t, a, "b::TestTwo").Instance)
}

// inFlightLog is a wireProxy whose settles keep to scopes
// (coreAgent.InFlightWindower) but cannot keep test workers' connections
// apart. log says, in order, each window begun ("window N", "window all"),
// each scope cut ("cut N", "cut all") and each read of a scope's work
// ("read N", "read all"): each holds none.
type inFlightLog struct {
	*wireProxy
	lmu sync.Mutex
	log []string
}

// workerInFlightProxy is an inFlightLog that keeps each test worker's
// connections apart (coreAgent.InFlightWorkerScoper).
type workerInFlightProxy struct{ *inFlightLog }

func (p *inFlightLog) note(s string) {
	p.lmu.Lock()
	defer p.lmu.Unlock()
	p.log = append(p.log, s)
}

func (p *inFlightLog) since(n int) []string {
	p.lmu.Lock()
	defer p.lmu.Unlock()
	return append([]string(nil), p.log[n:]...)
}

func (p *inFlightLog) InFlightScope() coreAgent.InFlightScope {
	p.note("cut all")
	return workerScope{p: p, of: "all"}
}

func (p *workerInFlightProxy) InFlightScopeOf(instance uint32) coreAgent.InFlightScope {
	p.note(fmt.Sprint("cut ", instance))
	return workerScope{p: p.inFlightLog, of: fmt.Sprint(instance)}
}

func (p *inFlightLog) InFlightBeginWindow() { p.note("window all") }

func (p *workerInFlightProxy) InFlightBeginWindowOf(instance uint32) {
	p.note(fmt.Sprint("window ", instance))
}

type workerScope struct {
	p  *inFlightLog
	of string
}

func (s workerScope) State(bool) (int, []string) {
	s.p.note("read " + s.of)
	return 0, nil
}
func (workerScope) PendingProgress(bool) map[uint64]uint64 { return nil }
func (workerScope) GiveUp([]uint64, bool) []string         { return nil }
func (workerScope) Behind() ([]string, func())             { return nil, func() {} }

// A test worker's test window is cut on its own worker's connections alone:
// its begin cuts where its window begins on them, its end waits for their
// work, and its worker's next begin waits for what that end left, never for
// another worker's. A cut on every connection would take a test another
// worker has open out of its end's wait, and its calls out of its
// recording.
func TestAWorkersWindowIsCutAndSettledOnItsOwnWorkersConnections(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	p := &workerInFlightProxy{&inFlightLog{wireProxy: a.Proxy.(*wireProxy)}}
	a.Proxy = p
	w1, w2 := child(t), child(t)
	require.Equal(t, "1", claimWorker(t, a, w1.Process.Pid, "a", 2))
	require.Equal(t, "2", claimWorker(t, a, w2.Process.Pid, "b", 2))
	step := func(pid int, name string, end bool) []string {
		t.Helper()
		n := len(p.since(0))
		if end {
			_, serr := a.ProvenEnd(as(pid), models.ScopeReq{Name: name, Outcome: "passed"})
			require.Nil(t, serr)
		} else {
			_, serr := a.ProvenBegin(as(pid), begin(name))
			require.Nil(t, serr)
		}
		return p.since(n)
	}
	require.Equal(t, []string{"window 1"}, step(w1.Process.Pid, "TestOne", false), "worker 1's first begin waits for nothing and cuts its own connections")
	end1 := step(w1.Process.Pid, "TestOne", true)
	require.Contains(t, end1, "cut 1", "worker 1's end waits for its own connections' work")
	require.Subset(t, []string{"cut 1", "read 1"}, end1, "and for no other worker's")
	require.Equal(t, []string{"window 2"}, step(w2.Process.Pid, "TestTwo", false), "worker 2's begin waits for none of worker 1's leftover work")
	end2 := step(w2.Process.Pid, "TestTwo", true)
	require.Contains(t, end2, "cut 2")
	require.Subset(t, []string{"cut 2", "read 2"}, end2)
	require.Equal(t, []string{"read 1", "window 1"}, step(w1.Process.Pid, "TestThree", false), "worker 1's next begin waits for what its own last end left")
}

// A proxy that cannot keep test workers' connections apart cuts no window
// for a worker's test, whose end then waits for every connection's work:
// more than the worker's calls, never fewer. A run of one test process cuts
// and waits on every connection, as before.
func TestAProxyThatCannotKeepWorkersApartCutsNoWorkersWindow(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	p := &inFlightLog{wireProxy: a.Proxy.(*wireProxy)}
	a.Proxy = p
	w1 := child(t)
	require.Equal(t, "1", claimWorker(t, a, w1.Process.Pid, "a", 2))
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	require.Empty(t, p.since(0), "no window is cut for a worker's test")
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: "passed"})
	require.Nil(t, serr)
	require.Contains(t, p.since(0), "cut all", "its end waits for every connection's work")

	b, _ := wireAgent(t, models.MODE_RECORD)
	q := &inFlightLog{wireProxy: b.Proxy.(*wireProxy)}
	b.Proxy = q
	claimSelf(t, b)
	_, serr = b.ProvenBegin(context.Background(), begin("TestOne"))
	require.Nil(t, serr)
	require.Equal(t, []string{"window all"}, q.since(0), "a run of one test process cuts every connection")
}

// A test worker's end names its worker to the bytes wait with its test
// (coreAgent.WithEndOfWorker): a wait cut short marks only that worker's
// connections (the proxy's waitPairTargets), so bytes another worker wrote
// and lost later stay its open test's, never charged to the ended test
// alone. Each end names its own worker, whichever ends first.
func TestAWorkersEndNamesItsWorkerToTheBytesWait(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	p := a.Proxy.(*wireProxy)
	w1, w2 := child(t), child(t)
	require.Equal(t, "1", claimWorker(t, a, w1.Process.Pid, "a", 2))
	require.Equal(t, "2", claimWorker(t, a, w2.Process.Pid, "b", 2))
	type end struct {
		test   string
		worker uint32
	}
	var ends []end
	p.bytesWait = func(ctx context.Context, _ uint64) error {
		if test := coreAgent.EndOf(ctx); test != "" {
			ends = append(ends, end{test, coreAgent.EndWorkerOf(ctx)})
		}
		return nil
	}
	_, serr := a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: "passed"})
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, []end{{"a::TestOne", 1}, {"b::TestTwo", 2}}, ends)
}

// refusedAlike claims as process pid what a refused claim asked (req), and
// requires the refusal it had (want), word for word: a refusal of the run's
// worker count is the run's, not one process's, so it names no claimant
// (neither pid nor any of others) and the claims of one run's test
// processes are one refusal, which keploy's end of run says once.
func refusedAlike(t *testing.T, a *Agent, req models.InstanceReq, pid int, want *models.ScopeError, others ...int) {
	t.Helper()
	_, serr := a.ClaimInstance(context.Background(), req, pid)
	require.NotNil(t, serr, "another test process of the run is refused too")
	require.Equal(t, want.Refusal(), serr.Refusal(), "another test process of the run is refused in the same words")
	for _, p := range append(others, pid) {
		require.NotContains(t, serr.Message, "process "+strconv.Itoa(p), "a refusal of the run's count names no claimant")
	}
}
