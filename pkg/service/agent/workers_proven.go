package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A run of more than one test worker (models.InstanceReq.Workers): each
// worker claims for itself, and keploy gives it an instance of its own, by
// the worker's kernel TGID (coreAgent.InstanceStamper), so the kernel stamps
// every connection the worker's processes make with that worker. A test's
// begin and end belong to the worker whose process sent them, read from the
// connection, never from what the harness says. How many workers a run may
// declare is MaxWorkers.

// MaxWorkers is how many test workers of one run this keploy takes at once:
// the one place that number is set. A claim that declares more is refused
// (models.ScopeErrTooManyWorkers) at once, whether or not its workers would
// have overlapped. This keploy runs one; a build that keeps each worker's
// calls apart sets it, once, before the agent serves.
var MaxWorkers = func() int { return 1 }

// workerClaim is a live worker's claim in a run of more than one worker: its
// process, its key and directory, and the worker count it declared. In a
// replay, es serves its tests and its calls outside them from a mock manager
// of the worker's own (serveWorker); nil in a record.
type workerClaim struct {
	claimant
	key, dir string
	declared int
	es       coreAgent.ExactServer
}

// declaredWorkers is how many test workers a claim of process caller says
// the run has: 0, from an integration that predates the count, and 1 both
// mean one. A count over the limit is refused in the same words whichever
// process sent it (it is the run's count, not the process's), so the
// claims of one run's test processes are one refusal, said once.
func declaredWorkers(n, caller int) (int, *models.ScopeError) {
	if n < 0 {
		return 0, scopeErr(http.StatusBadRequest, models.ScopeErrBadRequest,
			fmt.Sprintf("process %d declared %d test workers", caller, n),
			"send how many test workers the framework runs at once (1 or more), or leave it out for one")
	}
	if limit := MaxWorkers(); n > limit {
		most, remedy := fmt.Sprintf("at most %d test workers at once", limit), fmt.Sprintf("run at most %d test workers", limit)
		if limit <= 1 {
			most = "one test worker at a time"
			remedy = "run one worker (go test -p 1, Playwright workers: 1, pytest without -n, jest --runInBand)"
		}
		return 0, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrTooManyWorkers,
			fmt.Sprintf("a test process declared %d test workers, but this keploy runs %s", n, most), remedy)
	}
	return n, nil
}

// workerHooks is the hooks that put each of the run's declared test
// workers (more than one) in an instance of their own, or why this run takes
// no claim of declared test workers. Each reason holds for the whole run, so
// it is checked at the claim, before any test runs, never at a begin that
// happens to overlap another worker's test: whether a run is taken never
// depends on how its workers' tests are scheduled. For the same reason a
// refusal names no process: every test process of the run that claims is
// refused in the same words, one refusal said once.
func (a *Agent) workerHooks(ctx context.Context, declared int) (coreAgent.InstanceStamper, *models.ScopeError) {
	if serr := a.learnWorkersRefusal(declared); serr != nil {
		return nil, serr
	}
	if serr := a.clockWorkersRefusal(declared); serr != nil {
		return nil, serr
	}
	if serr := a.lossWorkersRefusal(ctx, declared); serr != nil {
		return nil, serr
	}
	return a.workerStamper(declared)
}

// takesWorkers says this run would take a claim of more than one test
// worker from process pid: the build runs them (MaxWorkers) and nothing of
// the run refuses them (workerHooks). A remedy names declaring the
// workers only where it says so.
func (a *Agent) takesWorkers(ctx context.Context, pid int) bool {
	if _, serr := declaredWorkers(2, pid); serr != nil {
		return false
	}
	_, serr := a.workerHooks(ctx, 2)
	return serr == nil
}

// learnWorkersRefusal refuses a claim as one of declared test
// workers in record's full round (models.OutgoingOptions.Learn): its learn
// step keeps the set's tests to the order they were recorded in, one after
// another (keepRecordedOrder, holdLearn), and tests that several workers run
// at once keep no order. A replay that learns nothing serves each worker on
// its own (serveWorker).
func (a *Agent) learnWorkersRefusal(declared int) *models.ScopeError {
	a.provenMu.Lock()
	learn := a.proven.order.learn
	a.provenMu.Unlock()
	if !learn {
		return nil
	}
	return scopeErr(http.StatusUnprocessableEntity, models.ScopeErrTooManyWorkers,
		fmt.Sprintf("a test process declared %d test workers, but record's full round replays one test worker at a time: its learn step pairs each call with a recording by the order the set's tests were recorded in, and tests that several workers run at once keep none", declared),
		"record with one worker (go test -p 1, Playwright workers: 1, pytest without -n, jest --runInBand)")
}

// clockWorkersRefusal refuses a claim as one of declared test
// workers in a replay whose build moves the run's clock to each test's
// recorded begin in this run (testClock): the run has one clock, so two
// workers' tests open at once cannot each run at their own recorded begin.
// It is refused at the claim, before any test runs, never at a begin that
// happens to overlap another worker's test: whether a run is taken never
// depends on how its workers' tests are scheduled.
func (a *Agent) clockWorkersRefusal(declared int) *models.ScopeError {
	if _, moves := testClock(); !moves || !a.replaying() {
		return nil
	}
	return scopeErr(http.StatusUnprocessableEntity, models.ScopeErrTooManyWorkers,
		fmt.Sprintf("a test process declared %d test workers, but this replay moves the run's clock to each test's recorded begin, and a run has one clock, so two workers' tests open at once cannot each run at their own", declared),
		"replay with one worker (go test -p 1, Playwright workers: 1, pytest without -n, jest --runInBand)")
}

// lossWorkersRefusal refuses a claim as one of declared test
// workers where keploy's hooks count no capture losses
// (countsCaptureLosses): a connection the kernel dropped while two workers'
// tests were open could then be charged to neither, and a test it belonged
// to could pass. One worker's test is open alone, and a drop in it is the
// run's gap or the one test's own, as before.
func (a *Agent) lossWorkersRefusal(ctx context.Context, declared int) *models.ScopeError {
	if a.countsCaptureLosses(ctx) {
		return nil
	}
	return scopeErr(http.StatusUnprocessableEntity, models.ScopeErrTooManyWorkers,
		fmt.Sprintf("a test process declared %d test workers, but keploy's hooks here count no capture losses, so a connection the kernel dropped while two workers' tests were open could not be charged to each of them", declared),
		"run one worker (go test -p 1, Playwright workers: 1, pytest without -n, jest --runInBand)")
}

// workerStamper is the hooks that put each of the run's declared test
// workers in an instance of its own (coreAgent.InstanceStamper), or why
// they cannot for any worker of the run: hooks that stamp no connection
// with an instance, or an agent in a PID namespace of its own, which cannot
// read the host's process ids the kernel keeps instances by. Like
// workerHooks's other reasons, it holds for the whole run and names no
// process.
func (a *Agent) workerStamper(declared int) (coreAgent.InstanceStamper, *models.ScopeError) {
	stamper, ok := a.Hooks.(coreAgent.InstanceStamper)
	if !ok {
		return nil, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrNotInInstance,
			fmt.Sprintf("a test process declared %d test workers, but keploy's hooks cannot tell one worker's calls from another's", declared),
			"run one worker")
	}
	if err := stamper.InstanceRefusal(); err != nil {
		return nil, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrNotInInstance,
			fmt.Sprintf("a test process declared %d test workers, but %v", declared, err),
			"run one worker (go test -p 1, Playwright workers: 1, pytest without -n, jest --runInBand), or run keploy's agent in the host's PID namespace")
	}
	return stamper, nil
}

// claimWorker claims an instance of the run for me, a worker of a run of
// declared workers, from the hooks workerHooks gave. With the wire held,
// after ClaimInstance's checks of the caller and endGoneWorkers.
func (a *Agent) claimWorker(ctx context.Context, req models.InstanceReq, me claimant, declared int, stamper coreAgent.InstanceStamper) (models.InstanceResp, *models.ScopeError) {
	a.provenMu.Lock()
	stale := ""
	if c := a.proven.claim; c.pid != 0 {
		if procAlive(c.pid, c.start) {
			a.provenMu.Unlock()
			return models.InstanceResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrWorkersMismatch,
				fmt.Sprintf("process %d declared %d test workers while process %d, which declared one, still runs this keploy run's tests", me.pid, declared, c.pid),
				"declare the same worker count from every test process the run starts at once")
		}
		// The run's single test process is gone: a test it left open can
		// no longer end, and fails. (No worker runs beside a single test
		// process, so an open test is that process's.)
		stale = a.proven.openOf(0)
		a.proven.claim = claimant{}
	}
	var live []string
	for _, id := range a.workerIDs() {
		w := a.proven.workers[id]
		if w.claimant == me {
			a.provenMu.Unlock()
			return models.InstanceResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrOneTestProcess,
				fmt.Sprintf("process %d claimed the run again: it is already test worker %d", me.pid, id),
				"claim once per test worker, before its first test")
		}
		if w.declared != declared {
			a.provenMu.Unlock()
			return models.InstanceResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrWorkersMismatch,
				fmt.Sprintf("process %d declared %d test workers, but test worker %d (process %d) declared %d", me.pid, declared, id, w.pid, w.declared),
				"declare the same worker count from every test process the run starts at once")
		}
		live = append(live, strconv.Itoa(w.pid))
	}
	if len(live) >= declared {
		a.provenMu.Unlock()
		return models.InstanceResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrOneTestProcess,
			fmt.Sprintf("process %d asked to run tests while %d test workers, as many as the run declared, still run them (processes %s)", me.pid, len(live), strings.Join(live, ", ")),
			"declare as many workers as the framework runs at once")
	}
	taken := make(map[uint32]bool, len(a.proven.workers))
	for id := range a.proven.workers {
		taken[id] = true
	}
	a.provenMu.Unlock()
	if stale != "" {
		a.failOpen(ctx, stale, "the test process exited while the test was open")
	}
	// The lowest id no live worker holds and no process is still in: one a
	// gone worker's survivor is still in is refused (coreAgent.ErrInstanceHeld),
	// and the next is tried.
	var id uint32
	for next := uint32(1); next <= coreAgent.MaxInstanceID; next++ {
		if taken[next] {
			continue
		}
		err := stamper.SetInstance(uint32(me.pid), next)
		if errors.Is(err, coreAgent.ErrInstanceHeld) {
			continue
		}
		if err != nil {
			return models.InstanceResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrNotInInstance,
				fmt.Sprintf("keploy could not give test worker process %d an instance of its own: %v", me.pid, err),
				"run one worker, or run keploy's agent in the host's PID namespace")
		}
		id = next
		break
	}
	if id == 0 {
		return models.InstanceResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrNotInInstance,
			fmt.Sprintf("keploy could not give test worker process %d an instance of its own: processes of gone workers are still in every one of its %d instances", me.pid, coreAgent.MaxInstanceID),
			"stop the processes earlier test workers left running")
	}
	es, serr := a.serveWorker(me.pid, id)
	if serr != nil {
		a.endInstances([]uint32{id})
		return models.InstanceResp{}, serr
	}
	a.provenMu.Lock()
	if a.proven.workers == nil {
		a.proven.workers = map[uint32]workerClaim{}
	}
	a.proven.workers[id] = workerClaim{claimant: me, key: req.Key, dir: req.Dir, declared: declared, es: es}
	a.provenMu.Unlock()
	if es != nil {
		// Calls the worker makes from now while none of its tests is open
		// are its key's.
		if err := a.servePoolExactly(ctx, id); err != nil {
			a.dropWorker(id)
			return models.InstanceResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
				fmt.Sprintf("keploy could not arm the recorded calls of %s made outside its tests for test worker process %d: %v", req.Key, me.pid, err), "check the agent's log")
		}
	}
	now := time.Now()
	a.provenMu.Lock()
	a.proven.claims = append(a.proven.claims, models.ScopeWindow{Claim: req.Key, Start: now, End: now, Worker: id, Workers: declared})
	a.provenMu.Unlock()
	containerkey.Default.BeginInstance()
	return models.InstanceResp{V: models.ScopeProtocolVersion, Instance: strconv.FormatUint(uint64(id), 10), TimeFile: TimeFile()}, nil
}

// serveWorker, in a replay, starts serving test worker id (process pid) from
// a mock manager of its own (coreAgent.InstanceServer), and returns how its
// tests are served there; nil in a record. A proxy that cannot is refused:
// one manager for every worker would let one worker's call answer from, or
// use up, another's mocks.
func (a *Agent) serveWorker(pid int, id uint32) (coreAgent.ExactServer, *models.ScopeError) {
	if !a.replaying() {
		return nil, nil
	}
	is, ok := a.Proxy.(coreAgent.InstanceServer)
	if !ok {
		return nil, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrNotInInstance,
			fmt.Sprintf("test worker process %d claimed a replay, but keploy's proxy cannot serve each test worker's calls from mocks of its own", pid),
			"replay with one worker (go test -p 1, Playwright workers: 1, pytest without -n, jest --runInBand)")
	}
	es, err := is.ServeInstance(id)
	if err != nil {
		return nil, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
			fmt.Sprintf("keploy could not serve test worker process %d's calls from mocks of its own: %v", pid, err), "check the agent's log")
	}
	return es, nil
}

// dropWorker takes back the claim of worker id, which no test of has begun:
// it is no worker, and its instance ends. With the wire held.
func (a *Agent) dropWorker(id uint32) {
	a.provenMu.Lock()
	delete(a.proven.workers, id)
	a.provenMu.Unlock()
	a.endInstances([]uint32{id})
}

// workerIDs are the live workers' instances, in order. With provenMu held.
func (a *Agent) workerIDs() []uint32 {
	ids := make([]uint32, 0, len(a.proven.workers))
	for id := range a.proven.workers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// endGoneWorkers ends the instance of every worker whose process exited: a
// test it left open fails, and its processes are no worker's from now on.
// With the wire held.
func (a *Agent) endGoneWorkers(ctx context.Context) {
	a.provenMu.Lock()
	var gone []uint32
	var stale []string
	for _, id := range a.workerIDs() {
		w := a.proven.workers[id]
		if procAlive(w.pid, w.start) {
			continue
		}
		gone = append(gone, id)
		delete(a.proven.workers, id)
		if open := a.proven.openOf(id); open != "" {
			stale = append(stale, open)
		}
	}
	a.provenMu.Unlock()
	for _, id := range stale {
		a.failOpen(ctx, id, "the test worker's process exited while the test was open")
	}
	a.endInstances(gone)
}

// endInstances takes every process out of instances ids. One the kernel
// could not end stays held: a later claim does not get it
// (coreAgent.ErrInstanceHeld). In a replay each one's manager goes first: a
// call its processes make from then on is refused, never answered from
// another worker's mocks.
func (a *Agent) endInstances(ids []uint32) {
	if is, ok := a.Proxy.(coreAgent.InstanceServer); ok && a.replaying() {
		for _, id := range ids {
			// A new session's proxy serves no worker any more: an id it
			// does not serve is already stopped.
			if err := is.StopServingInstance(id); err != nil {
				a.logger.Debug("a test worker's instance was not served", zap.Uint32("instance", id), zap.Error(err))
			}
		}
	}
	stamper, ok := a.Hooks.(coreAgent.InstanceStamper)
	if !ok {
		return
	}
	for _, id := range ids {
		if _, err := stamper.EndInstance(id); err != nil {
			a.logger.Warn("keploy could not end a test worker's instance; no later worker gets it", zap.Uint32("instance", id), zap.Error(err))
		}
	}
}

// callerWorker is, in a run of more than one worker, the instance of the
// worker whose process sent what (a begin or an end of test name); 0 in a run
// of one. A process that is no live worker's claimant is refused: its test
// would be charged to a worker by guess.
func (a *Agent) callerWorker(ctx context.Context, what, name string) (uint32, *models.ScopeError) {
	a.provenMu.Lock()
	n := len(a.proven.workers)
	a.provenMu.Unlock()
	if n == 0 {
		return 0, nil
	}
	read := coreAgent.CallerOf(ctx)
	if read == nil {
		return 0, scopeErr(http.StatusInternalServerError, models.ScopeErrNoCaller,
			"keploy's agent could not tell which test worker sent "+name+"'s "+what, "run the test command on the machine keploy's agent runs on")
	}
	pid, err := read()
	if err != nil {
		return 0, scopeErr(http.StatusInternalServerError, models.ScopeErrNoCaller,
			"keploy's agent could not tell which test worker sent "+name+"'s "+what+": "+err.Error(), "run the test command on the machine keploy's agent runs on")
	}
	// A process that is gone reads as start 0, which no worker's is: each
	// was read alive at its claim.
	start, _ := procStart(pid)
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	for _, id := range a.workerIDs() {
		if w := a.proven.workers[id]; w.pid == pid && w.start == start {
			return id, nil
		}
	}
	return 0, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrNotInInstance,
		fmt.Sprintf("process %d sent %s's %s, but it is not a live test worker of this run, so keploy cannot tell which worker's test it is", pid, name, what),
		"send each test's begin and end from the test worker that claimed the run")
}
