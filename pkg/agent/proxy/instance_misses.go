package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// missWindow is one misses window: the run's (the proxy's own) or a test
// worker's (instanceServing.misses). The error drain files each miss in the
// window of the worker whose process made it (missWindowOf), so a worker's
// test reads its own misses and never another worker's.
type missWindow struct {
	// activeTestErrors accumulates mock-not-found errors during active test
	// execution. The continuous drain goroutine routes errors here when non-nil.
	// When nil (no window), mock-not-found errors fall to pendingMockErrors and
	// only non-mock noise (OTel, health checks) is discarded — keeping the
	// errChannel drained either way.
	activeTestErrors atomic.Pointer[testErrorAccumulator]
	// missLoss is why a miss was lost since the last read (the first cause):
	// it could not be queued (the error drain had stopped, or never ran) or
	// its test's list was full. The next GetMockErrors then reports its read
	// incomplete (ErrUnmatchedCallsUnread), naming the cause, instead of
	// returning a list a miss is missing from.
	missLoss atomic.Pointer[string]
	// pendingLost is set, under captureMu, when a miss made with no test's
	// window open was dropped because pendingMockErrors was full. The next
	// window decides whose loss it is: a set's first discards those misses
	// (they are no test's), so it only says so, but under the proven switch,
	// where they are the set's (beforeSetMiss) and its read is incomplete
	// (missLoss); the next test's carries them, so its read is incomplete.
	pendingLost bool
	// pendingMockErrors retains mock-not-found errors that arrive while no test
	// capture window is active (activeTestErrors is nil) — e.g. between tests,
	// or when the replayer/agent predates the BeginTestErrorCapture wiring.
	// ContinueTestErrorCapture carries them into the next test's window;
	// BeginTestErrorCapture (a set's first test) discards and logs them, but
	// under the proven switch, where it carries them as the set's
	// (beforeSetMiss);
	// GetMockErrors returns them where no window is opened at all. Only
	// mock-not-found errors are retained (background noise — OTel, health
	// checks — is still discarded), and the buffer is bounded so it can't grow
	// unboundedly.
	pendingMockErrors testErrorAccumulator
	// setStaged is, under the proven switch, the staging
	// (MockManager.StagingEpoch) of the running test set's first on the
	// manager this window's calls are marked on (the run's, or the test
	// worker's own), which cut what was missed until then
	// (cutMissesAtSetStart); 0 before any. A miss whose call was marked in an
	// earlier staging (models.MockMismatchReport.MarkEpoch) is not the set's,
	// and the drain cuts it as the set's start did, however late it arrives
	// (markedBeforeSet), or keeps it as the start did, one marked in a test's
	// view (markedInATest), or one its set's reads of this window would have
	// taken (keptAsEarlierSet).
	setStaged atomic.Uint64
	// starts are, under the proven switch, the test sets' starts this
	// window's calls were marked against, oldest first (noteSetStart): where
	// each set's mocks were staged on the manager its calls are marked on, and
	// how many reads of this window had begun by then. Read and written under
	// startsMu.
	startsMu sync.Mutex
	starts   []setStart
}

// setStart is a test set's start on a misses window (cutMissesAtSetStart):
// the staging of the set's mocks on the manager the window's calls are marked
// on (MockManager.StagingEpoch), how many reads of the window had begun by
// then (MockManager.windowReads), so the previous set's last, and the set's
// place in the proxy's count of set starts (Proxy.setStarts).
type setStart struct {
	epoch, reads, set uint64
}

// noteSetStart notes s, a test set's start, on w (missWindow.starts).
func (w *missWindow) noteSetStart(s setStart) {
	w.startsMu.Lock()
	w.starts = append(w.starts, s)
	w.startsMu.Unlock()
}

// keptAsEarlierSet reports, for err, a miss filed in w whose call was marked
// before the running test set's staging (markedBeforeSet), whether it is kept
// as an earlier set's call (earlierSetMiss): one marked in a test's view
// (markedInATest), that test's own; or one marked while that set's mocks
// served (models.RoundMarkedSettled), with no test's view named (the pool
// view between two of its tests, after its last, or a view the store names no
// test of), before that set's last read of w (models.MockMismatchReport.
// MarkReads), which at zero latency that read, or an earlier one, would have
// returned for keploy test to fail the set with. A call marked while a set's
// mocks were being staged, before a set's first staging on w, or after its
// set's last read of w, which at zero latency the next set's start cuts too,
// is not kept, nor is one whose parser marks no round.
func (w *missWindow) keptAsEarlierSet(err error) bool {
	if markedInATest(err) {
		return true
	}
	var pe models.ParserError
	if !errors.As(err, &pe) || pe.MismatchReport == nil || pe.MismatchReport.Mark != models.RoundMarkedSettled {
		return false
	}
	r := pe.MismatchReport
	w.startsMu.Lock()
	defer w.startsMu.Unlock()
	k := w.startOfLocked(r.MarkEpoch)
	if k < 0 || k+1 >= len(w.starts) {
		return false
	}
	return r.MarkReads < w.starts[k+1].reads
}

// setsBack is, for err, an earlier set's call read in w (earlierSetMiss), how
// many test sets before the running one its call was marked in: 1 for the
// set that ran just before, and 1 where w keeps no start before the mark's.
func (w *missWindow) setsBack(err error, running uint64) int {
	var pe models.ParserError
	if !errors.As(err, &pe) || pe.MismatchReport == nil {
		return 1
	}
	w.startsMu.Lock()
	defer w.startsMu.Unlock()
	k := w.startOfLocked(pe.MismatchReport.MarkEpoch)
	if k < 0 || w.starts[k].set >= running {
		return 1
	}
	return int(running - w.starts[k].set)
}

// startOfLocked is the index in w.starts of the set start a call marked in
// staging epoch belongs to (the last one staged at or before it), -1 for none.
// Called under startsMu.
func (w *missWindow) startOfLocked(epoch uint64) int {
	k := -1
	for i, s := range w.starts {
		if s.epoch > epoch {
			break
		}
		k = i
	}
	return k
}

// missInstance is the test worker whose process made the miss err (0: none).
// errors.As reaches a miss carried as the set's through beforeSetMiss.Unwrap.
func missInstance(err error) uint32 {
	var pe models.ParserError
	if errors.As(err, &pe) {
		return pe.Instance
	}
	return 0
}

// missWindowOf is the misses window of test worker inst: the run's for 0,
// nil for a worker keploy does not serve (it ended, or never began).
func (p *Proxy) missWindowOf(inst uint32) *missWindow {
	if inst == 0 {
		return &p.missWindow
	}
	s := &p.instances
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.misses[inst]
}

// stoppedWorker reports whether a call of test worker inst is one keploy no
// longer serves, as it is made: test workers are served, and inst, which
// names one, is not among them (it ended). Its connection was made before
// then: keploy refuses one made after (connManager).
func (p *Proxy) stoppedWorker(inst uint32) bool {
	if inst == 0 {
		return false
	}
	s := &p.instances
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.on && s.mgrs[inst] == nil
}

// stoppedWorkerMissWhat names a miss of stopped test worker inst, made by
// process pid, as a call keploy cannot attribute (stoppedWorker): where it
// went, as its parser or its dial named it, and the ended worker.
func stoppedWorkerMissWhat(r *models.MockMismatchReport, dst *models.ConditionalDstCfg, pid, inst uint32) string {
	to := ""
	switch {
	case r != nil && r.Destination != "":
		to = " to " + r.Destination
	case dst != nil && dst.Addr != "" && !dst.AddrFabricated:
		to = " to " + dst.Addr
	}
	return fmt.Sprintf("the latest: a call%s, from process %d of ended test worker %s, on a connection it made before keploy stopped serving that worker", to, pid, agent.InstanceLabel(inst))
}

// workersServed reports whether test workers are served: from the first
// worker served until the run's manager is replaced (endInstances).
func (p *Proxy) workersServed() bool {
	s := &p.instances
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.on
}

// sendDNSMiss reports miss, a DNS question no DNS mock answers, as it is
// asked. A DNS question carries no kernel stamp of the process that asked it
// (only the TCP connect hooks stamp a worker's instance), so while test
// workers are served keploy cannot tell whose test it is: it is counted as a
// call keploy cannot attribute (models.CaptureLossUnattributed), which leaves
// each test open then without a verdict. Filed in the run's window, which no
// worker's test reads, it would fail no test. Before any worker is served it
// is the run's test's miss, as before.
func (p *Proxy) sendDNSMiss(miss error, question dns.Question) {
	if p.workersServed() {
		p.losses.Add(models.CaptureLossUnattributed, fmt.Sprintf("the latest: a DNS question for %s (%s) that no DNS mock answers, asked while test workers are served: a DNS question carries no stamp of the test worker that asked it", question.Name, dns.TypeToString[question.Qtype]))
		return
	}
	p.SendError(miss)
}

// markedOn is a misses window with the manager its calls are marked on.
type markedOn struct {
	w *missWindow
	m *MockManager
}

// missWindowsMarkedOn is the run's misses window, with the run's manager,
// and each served test worker's, with the worker's own manager (nil for
// none): a worker's call is marked on its own manager's stagings, never the
// run's (setStaged).
func (p *Proxy) missWindowsMarkedOn() []markedOn {
	out := []markedOn{{&p.missWindow, p.getMockManager()}}
	s := &p.instances
	s.mu.RLock()
	defer s.mu.RUnlock()
	for inst, w := range s.misses {
		out = append(out, markedOn{w, s.mgrs[inst]})
	}
	return out
}

// managerOf is the manager the calls whose misses w files are marked on
// (missWindowsMarkedOn): the run's for the run's window, a served test
// worker's own for its, nil for none.
func (p *Proxy) managerOf(w *missWindow) *MockManager {
	for _, o := range p.missWindowsMarkedOn() {
		if o.w == w {
			return o.m
		}
	}
	return nil
}

// missManager is the manager that served the call a miss of test worker
// inst was made on: the run's for 0, nil for a worker keploy does not serve.
func (p *Proxy) missManager(inst uint32) *MockManager {
	if inst == 0 {
		return p.getMockManager()
	}
	return p.instanceManager(inst)
}

// ContinueTestErrorCapture opens the window of the worker's next test, as
// the proxy's own does the run's (Proxy.ContinueTestErrorCapture).
func (s instanceExact) ContinueTestErrorCapture() {
	if w := s.p.missWindowOf(s.inst); w != nil {
		s.p.openCaptureOn(w, true, false)
	}
}

// WithdrawTestErrorCapture takes back the window of a worker's test that
// did not begin (Proxy.WithdrawTestErrorCapture).
func (s instanceExact) WithdrawTestErrorCapture() {
	if w := s.p.missWindowOf(s.inst); w != nil {
		s.p.withdrawCaptureOn(w)
	}
}

// GetMockErrors reads the misses of the worker's test (Proxy.GetMockErrors):
// its own process tree's, never another worker's. A worker keploy no longer
// serves has none to read, which is an error.
func (s instanceExact) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	w := s.p.missWindowOf(s.inst)
	if w == nil {
		return nil, instanceNotServed(s.inst)
	}
	return s.p.getMockErrorsOn(ctx, w)
}
