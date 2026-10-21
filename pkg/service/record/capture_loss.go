package record

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// errCaptureCountSilent is why, under the proven switch, a read of the
// agent's count was given up: keploy had the read's whole bound
// (stall.HangBudget of its own runnable time) to run, and the agent did not
// answer.
var errCaptureCountSilent = fmt.Errorf("the keploy agent did not say whether it captured every call: keploy had %s of its own time to run, and it did not answer", stall.HangBudget)

// errCaptureCountStopped is why a read the user's Ctrl+C ended (at an offer
// of the stop's: utils.StopNow) is not read.
var errCaptureCountStopped = errors.New("a Ctrl+C stopped keploy's wait for its agent to say whether it captured every call")

// errCaptureCountNotRead is why a test case whose read had not come when the
// recording ended (it was asked as finish began: a consumer of a record
// drain that gave up) is not verified, and the final read, when it was asked
// and no read since answered or failed.
var errCaptureCountNotRead = errors.New("keploy's read of whether its agent captured every call had not come when the recording ended")

// CaptureLossReader is an optional Instrumentation extension: every kind of
// capture loss the agent has counted since it started (models.CaptureLosses).
type CaptureLossReader interface {
	GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error)
}

// captureWatch is, under the proven switch, keploy record's account of what
// the agent could not capture (design v14 §3.2, rule 12): processes the
// kernel refused a place in the agent's PID maps, client-side TLS handshakes
// with keploy's proxy that failed other than by the client hanging up, TLS
// upgrades keploy could not make, counted from the moment the agent began
// capturing. A loss taints the test case being recorded when it happened and
// every later one -- but for a test case's own kinds (models.PerTest), which
// are the test case's they happened in -- so each test case is checked as it
// is saved: every one whose check sees a loss is named at the end, and the
// recording fails. A test case whose check could not be read is named as
// unverified, and fails the recording too. Where a later pass judges each
// test case (JudgeLossesLater), it fails nothing, and hands what it found to
// that pass. With the switch off, or an agent that cannot count, it does
// nothing.
//
// The checks are countReads' (reads): the base read as capture begins, waited
// for; then each test case's, asked as it is saved and not waited for, and
// the final one, asked as the stop ends the capture; one at a time, in the
// order asked. None is ended by the stop (only by the interrupt that gives
// the stop's waits up, utils.StopNow), and each is given up once keploy had
// stall.HangBudget of its own runnable time to run without an answer, so
// an agent that never answers holds no read, and no stop, for longer: what a
// read given up would have checked is then not verified, and the recording
// fails, saying so; every test case and mock it saved is kept. A read that
// lags counts every test case saved since the last together, each charged
// with what it saw. While one asked is out, or a test case the stop's drain
// handed over is still being saved, its check yet to be asked, the stop's
// frame drains hold the agent's streams open (Recorder.holdForReads): an
// agent the stop reached too goes once they end, and a read asked of it then
// would be refused.
type captureWatch struct {
	logger *zap.Logger
	off    bool
	base   models.CaptureLosses
	// last is the count as the previous test case was checked, which a test
	// case's own kinds (models.PerTest) count from; nil before the first.
	last models.CaptureLosses
	// baseErr is why the count could not be read when capture began; every
	// test case is then unverified, and so is the recording's end (the final
	// read is not made: afterTestCases).
	baseErr error
	// reads makes the test cases' and the final reads, under w.mu; nil with
	// the switch off, or once the base read failed.
	reads *countReads

	mu         sync.Mutex
	tainted    []string
	unverified []string
	lost       models.CaptureLosses
	readErr    error
	// uncapturable are the test cases recorded while or after the
	// application called what keploy cannot capture at all (decision 20),
	// and notJudged what that was: said, never a failure.
	uncapturable []string
	notJudged    models.CaptureLosses
	// charged is the most the test cases' checks saw lost (judged kinds),
	// kind by kind; final is what the final read (afterTestCases) saw lost,
	// whose excess over charged no test case is charged with (a test case
	// the drain saves after the final read is checked, and charged, after
	// it); finalErr is why that read failed.
	charged  models.CaptureLosses
	final    models.CaptureLosses
	finalErr error
	// finalRead: a read made once the final read was asked answered (final
	// is its count); finalErr is then nil.
	finalRead bool
	// chargedNotJudged and finalNotJudged are charged's and final's
	// counterparts for what leaves a test case without a verdict: an
	// abandoned TLS handshake beyond the most the checks saw was after the
	// last test case, said (a Unix socket's is every test case's from the
	// first on already).
	chargedNotJudged models.CaptureLosses
	finalNotJudged   models.CaptureLosses
	// evidenced are the test cases recorded while or after the kernel
	// skipped runs of keploy's capture programs, and evidence what it
	// skipped so far (models.CaptureLossKind.Evidence); chargedEvidence and
	// finalEvidence are its charged and final counterparts. Evidence only,
	// never a verdict: said, and no failure.
	evidenced       []string
	evidence        models.CaptureLosses
	chargedEvidence models.CaptureLosses
	finalEvidence   models.CaptureLosses
	// unreadMocks is each saved mock, by name, whose stamp says the Docker
	// daemon did not answer keploy's read of its container's configuration
	// (models.ContainerUnreadMetadata), with the container named;
	// unreadMapped, those a test case's mapping held. A record charges such
	// a mock's own test case, by its mapping, whenever the read stalled: the
	// count of the kind (models.CaptureLossContainerUnread), which may come
	// after the test case was checked, is left out of every check.
	unreadMocks  map[string]string
	unreadMapped map[string]bool
	unreadLost   models.CaptureLosses
	// ca is, with the proven switch off, the WARN of the TLS handshakes a
	// client failed with keploy's certificate (caRejectWatch); nil with it on.
	ca *caRejectWatch
	// byTestCase is what each test case's check charged it with, judged
	// and not judged kinds alike, and the containers its mapping named the
	// daemon did not answer the read of (mapped): the evidence a later
	// judging pass reads (RecordedLosses).
	byTestCase map[string]models.CaptureLosses
	// agent watches which agent process answers (models.AgentInstanceWatch):
	// the counts are the process's, from when it started, and the test cases
	// and mocks it captured are in its memory until it hands them over. An
	// agent started again in its place mid-recording (kubelet restarting a
	// crashed sidecar) counts from nothing, and what the one before held went
	// with it. nil when the instrumentation cannot tell.
	agent models.AgentInstanceWatch
}

// agentInstanceWatcher is an Instrumentation that can tell which agent
// process answers it (models.AgentInstanceWatch): the keploy agent client.
type agentInstanceWatcher interface {
	WatchAgentInstance() models.AgentInstanceWatch
}

// watchCaptureLosses marks the count the recording's test cases are checked
// against. Called once the agent is capturing and before the app starts.
func watchCaptureLosses(ctx context.Context, logger *zap.Logger, instr Instrumentation) *captureWatch {
	w := &captureWatch{logger: logger}
	reader, ok := instr.(CaptureLossReader)
	if !provenswitch.On() || !ok {
		w.off = true
		if ok {
			// With the switch off a failed TLS handshake is only said, at
			// WARN, never a failure.
			w.ca = newCARejectWatch(ctx, logger, reader)
		}
		return w
	}
	// Begun before the count is read, so the agent that answers it is the
	// one the recording's checks keep to.
	if a, ok := instr.(agentInstanceWatcher); ok {
		w.agent = a.WatchAgentInstance()
	}
	w.reads = newCountReads(ctx, logger, reader, &w.mu, errCaptureCountSilent,
		"whether it captured every call while this recording ran",
		"the test cases it would check are then not verified, and the recording fails",
		w.counted)
	// Waited for, but not ended by the stop: from here a stop drains what
	// the agent captured (Start), and each test case it saves is checked
	// against this count, so a stop that cut the read short would fail the
	// recording the user stopped as "not verified". An agent that does not
	// answer is given up on its bound (countReads.readNow), and every test
	// case is then not verified.
	base, err := w.reads.readNow()
	switch {
	case errors.Is(err, models.ErrCaptureLossesUnsupported):
		logger.Debug("the agent cannot say whether it captured every call; capture completeness is not checked", zap.Error(err))
		w.off = true
		w.reads = nil
		if w.agent != nil {
			w.agent.Stop()
			w.agent = nil
		}
	case err != nil:
		if errors.Is(err, context.Canceled) {
			// Its context is stop-proof: only utils.StopNow ends it.
			err = errCaptureCountStopped
		}
		w.baseErr = err
		w.reads = nil
	default:
		w.base = base.Without(models.CaptureLossContainerUnread)
	}
	return w
}

// afterTestCase asks for the check of test case name, just saved
// (countReads.askTest), and does not wait for it: an agent-life loss since
// the recording began taints it, and a failed TLS handshake since the
// previous check (models.PerTest) is its own (counted).
func (w *captureWatch) afterTestCase(ctx context.Context, name string) {
	if w == nil {
		return
	}
	if w.off {
		w.ca.afterTestCase(ctx, name)
		return
	}
	if w.baseErr != nil {
		w.mu.Lock()
		w.unverified = append(w.unverified, name)
		w.mu.Unlock()
		return
	}
	if askingCheck != nil {
		askingCheck(name)
	}
	w.reads.askTest(name)
}

// counted takes a check's answer (countReads.apply), w.mu held. Its test
// cases (more than one when the reads lag) are each charged with what it
// saw: one that failed, or was given up, leaves them not verified, and the
// next one counts from the check before. One made once the final read was
// asked is the final read's answer too (afterTestCases).
func (w *captureWatch) counted(r countRead) {
	if r.err != nil {
		err := r.err
		if errors.Is(err, context.Canceled) {
			// Its context is stop-proof: only utils.StopNow ends it.
			err = errCaptureCountStopped
		}
		w.unverified = append(w.unverified, r.names...)
		if len(r.names) > 0 && w.readErr == nil {
			w.readErr = err
		}
		if r.afterFinal && !w.finalRead {
			w.finalErr = err
		}
		return
	}
	now := r.losses.Without(models.CaptureLossContainerUnread) // charged by its mock (mapped)
	if len(r.names) > 0 {
		test := w.last
		if test == nil {
			test = w.base
		}
		w.last = now
		charged := now.Charged(w.base, w.base, test)
		if len(charged) > 0 {
			for _, name := range r.names {
				w.addEvidence(name, charged)
			}
		}
		judged, notJudged := charged.Split()
		// Every loss so far: the reason names them all, not only these test
		// cases' own (an earlier one's failed handshake is still a reason the
		// recording is incomplete), and each was charged to a test case by now.
		allJudged, allNotJudged := now.Since(w.base).Split()
		allEvidence := now.Since(w.base).Evidence()
		w.charged = w.charged.Max(allJudged)
		w.chargedNotJudged = w.chargedNotJudged.Max(allNotJudged)
		w.chargedEvidence = w.chargedEvidence.Max(allEvidence)
		if len(charged.Evidence()) > 0 {
			w.evidenced = append(w.evidenced, r.names...)
			w.evidence = allEvidence
		}
		if len(judged) > 0 {
			w.tainted = append(w.tainted, r.names...)
			w.lost = allJudged
		}
		if len(notJudged) > 0 {
			w.uncapturable = append(w.uncapturable, r.names...)
			w.notJudged = allNotJudged
		}
	}
	if r.afterFinal {
		w.final, w.finalNotJudged = now.Since(w.base).Split()
		w.finalEvidence = now.Since(w.base).Evidence()
		w.finalRead, w.finalErr = true, nil
	}
}

// saved notes a mock as it is saved: one whose stamp says the Docker daemon
// did not answer keploy's read of its container's configuration is its own
// test case's to charge, once a mapping names it (mapped). Called before the
// mock is published to the mappings.
func (w *captureWatch) saved(m *models.Mock) {
	if w == nil || w.off || m == nil || m.Name == "" {
		return
	}
	what := m.Spec.Metadata[models.ContainerUnreadMetadata]
	if what == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.unreadMocks == nil {
		w.unreadMocks = map[string]string{}
	}
	w.unreadMocks[m.Name] = what
}

// mapped charges test case test with each of its mocks (entries, as its
// mapping resolved them) whose container the Docker daemon did not answer
// the read of (saved): the test case is not judged, the container named,
// whenever the mapping came, before or after the test case was checked, and
// no other test case is charged with it.
func (w *captureWatch) mapped(test string, entries []models.MockEntry) {
	if w == nil || w.off {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range entries {
		what, ok := w.unreadMocks[e.Name]
		if !ok || w.unreadMapped[e.Name] {
			continue
		}
		if w.unreadMapped == nil {
			w.unreadMapped = map[string]bool{}
		}
		w.unreadMapped[e.Name] = true
		if !slices.Contains(w.uncapturable, test) {
			w.uncapturable = append(w.uncapturable, test)
		}
		lost := models.CaptureLosses{{Kind: models.CaptureLossContainerUnread, Count: 1, What: what}}
		w.unreadLost = w.unreadLost.Plus(lost)
		w.addEvidence(test, lost)
	}
}

// addEvidence adds lost to what test case name was charged with. Called
// with w.mu held.
func (w *captureWatch) addEvidence(name string, lost models.CaptureLosses) {
	if w.byTestCase == nil {
		w.byTestCase = map[string]models.CaptureLosses{}
	}
	w.byTestCase[name] = w.byTestCase[name].Plus(lost)
}

// afterTestCases asks for the final read, once the app is stopped and before
// the agent may stop (countReads.askFinal), and does not wait for it: what
// was lost after the last test case was checked is charged to no test case,
// so no test case's mocks miss it, and the recording fails on it (design v14
// §7.5: evidence charged to no test). Without it a loss after the last test
// case went unsaid. A test case's check made after it (the drain saves the
// last test cases after it in a native or docker stop) charges that test
// case instead.
func (w *captureWatch) afterTestCases(ctx context.Context) {
	if w == nil {
		return
	}
	if w.off {
		w.ca.afterTestCases(ctx)
		return
	}
	if w.baseErr != nil {
		// No count from the start to judge it against: what was lost after
		// the last test case is not verified either, and a recording that
		// saved no test case fails for it, as one whose final read failed.
		w.mu.Lock()
		if w.finalErr == nil {
			w.finalErr = w.baseErr
		}
		w.mu.Unlock()
		return
	}
	w.reads.askFinal()
}

// waitForReads waits for the reads still out (countReads.waitForReads, and
// the CA watch's with the switch off): once the frame drains are over, while
// the agent is up.
func (w *captureWatch) waitForReads() {
	if w == nil {
		return
	}
	if w.off {
		w.ca.waitForReads()
		return
	}
	w.reads.waitForReads()
}

// finish waits for the checks still out (countReads.wait, offering one more
// Ctrl+C: every drain is over), then says, at ERROR, which test cases were
// recorded while the agent could not capture every call, or could not be
// checked, and reports whether the recording is to fail for it: for that, or
// for realUncaptured (connections to a mock.real destination keploy could
// neither capture nor relay). A check that had not come, or the final read,
// is not verified: unknown is not none. Where a later pass judges each
// recorded test case (JudgeLossesLater), it fails nothing: what it would
// fail on it says in one WARN, and hands to that pass with the rest
// (RecordedLosses). What fails nothing either way (a call keploy cannot
// capture, an abandoned handshake, a container not named, data passed
// through, runs of keploy's programs the kernel skipped, a process keploy
// could not classify) is said as it always is.
func (w *captureWatch) finish(testSetID string, realUncaptured bool) bool {
	fail, _ := w.finishWith(testSetID, realUncaptured, "")
	return fail
}

// finishRun is finish as a recording's end takes it, with realUnread, why
// keploy could not read which mock.real connections it could neither
// capture nor relay ("" when it could), which fails it as realUncaptured
// does: whether the run fails, and whether the recording is incomplete,
// which it is too when what would have failed it was handed to a later
// pass.
func (w *captureWatch) finishRun(testSetID string, realUncaptured bool, realUnread string) (fail, incomplete bool) {
	fail, handed := w.finishWith(testSetID, realUncaptured, realUnread)
	return fail, fail || handed
}

// finishWith is finish with realUnread (finishRun), and whether what it
// handed a later pass holds what fails a recording judged on its own
// (RecordedLosses.Failing). A set is handed over when the watch ran, and,
// where it did not (an agent that cannot count, the switch off), only for
// mock.real connections lost or unread.
func (w *captureWatch) finishWith(testSetID string, realUncaptured bool, realUnread string) (fail, handed bool) {
	judge := lossJudge.Load()
	if w == nil || w.off {
		if w != nil {
			w.ca.finish(testSetID) // said, never a failure
		}
		if judge == nil || !realUncaptured && realUnread == "" {
			return realUncaptured || realUnread != "", false
		}
		// Nothing was counted (an agent that cannot count): the mock.real
		// connections are all there is to hand over.
		l := RecordedLosses{TestSetID: testSetID, RealUncaptured: realUncaptured, RealUnread: realUnread}
		(*judge)(l)
		return false, l.Failing()
	}
	w.reads.wait(true)
	w.mu.Lock()
	failed := realUncaptured || realUnread != ""
	// Unlocked before a judge is handed what was found.
	locked := true
	defer func() {
		if locked {
			w.mu.Unlock()
		}
	}()
	if w.reads != nil {
		w.reads.finished = true
		if unread := w.reads.unreadLocked(); len(unread) > 0 {
			w.unverified = append(w.unverified, unread...)
			if w.readErr == nil {
				w.readErr = errCaptureCountNotRead
			}
		}
		if w.reads.finalAsked && !w.finalRead && w.finalErr == nil {
			w.finalErr = errCaptureCountNotRead
		}
	}
	if len(w.tainted) > 0 && judge == nil {
		failed = true
		w.logger.Error("the recording is incomplete: keploy's agent could not capture every call while these test cases were recorded, so their calls may be missing from the mocks ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Strings("testCases", w.tainted),
			zap.String("reason", w.lost.Text()),
			zap.String("next_step", "fix what the reason names, then re-record these test cases"))
	}
	if len(w.uncapturable) > 0 {
		notJudged := w.notJudged.Plus(w.unreadLost)
		// Not a failure: keploy never had these calls to lose, or cannot
		// tell it lost one (an abandoned TLS handshake), or which container
		// one reached (a container read the Docker daemon did not answer).
		// The test cases are kept; the reason says which.
		w.logger.Warn("these test cases' calls may be missing from the mocks, or not be their container's: each was recorded during or after what the reason names, one of a call the application made that keploy cannot capture, a TLS client that abandoned its handshake, a container the Docker daemon did not answer keploy's read of, or a process keploy could not classify ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Strings("testCases", w.uncapturable),
			zap.String("reason", notJudged.NotJudgedText()),
			zap.String("next_step", notJudged.NotJudgedRemedy()+", then re-record these test cases"))
	}
	if len(w.evidenced) > 0 {
		// Evidence only (the kernel's recursion_misses, 5.12+): a kernel
		// without the count records the same, and nothing fails.
		w.logger.Warn("the kernel skipped runs of keploy's capture programs while these test cases were recorded: evidence, not a verdict; a call of theirs may be missing from the mocks, which their replay shows ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Strings("testCases", w.evidenced),
			zap.String("reason", w.evidence.EvidenceText()),
			zap.String("next_step", models.KernelSkipRemedy))
	}
	var unmapped models.CaptureLosses
	for name, what := range w.unreadMocks {
		if !w.unreadMapped[name] {
			unmapped = unmapped.Plus(models.CaptureLosses{{Kind: models.CaptureLossContainerUnread, Count: 1, What: what}})
		}
	}
	if len(unmapped) > 0 {
		// Mocks no test case's mapping holds (the application's start, or
		// background calls): no test case is charged, and nothing fails.
		w.logger.Warn("keploy could not name the container of mocks no test case holds, because the Docker daemon did not answer keploy's read of it: they are kept without its key ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.String("reason", unmapped.NotJudgedText()),
			zap.String("next_step", unmapped.NotJudgedRemedy()+", then re-record"))
	}
	// A handshake abandoned after the last test case was checked, or data
	// sent over a server-first protocol keploy relays: charged to no test
	// case, and said, never a failure.
	var abandonedLate, relayedLate models.CaptureLosses
	for _, c := range w.finalNotJudged.Since(w.chargedNotJudged) {
		switch {
		case c.Kind == models.CaptureLossServerFirst || c.Kind == models.CaptureLossRefusedAtAccept:
			relayedLate = append(relayedLate, c)
		case c.Kind.PerTest():
			abandonedLate = append(abandonedLate, c)
		}
	}
	if skippedLate := w.finalEvidence.Since(w.chargedEvidence); len(skippedLate) > 0 {
		w.logger.Warn("the kernel skipped runs of keploy's capture programs after the last test case was recorded: evidence, not a verdict; no test case holds what they may have missed ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.String("reason", skippedLate.EvidenceText()),
			zap.String("next_step", models.KernelSkipRemedy))
	}
	if len(relayedLate) > 0 {
		w.logger.Warn("the application sent data on connections keploy passes through (a server-first protocol, or a destination's refusal), after the last test case was recorded: no test case holds those calls ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.String("reason", relayedLate.NotJudgedText()),
			zap.String("next_step", relayedLate.NotJudgedRemedy()+", then re-record"))
	}
	if len(abandonedLate) > 0 {
		// Most often a connection the application dialled, never used and
		// closed at its stop (Go's http.Transport keeps such a spare): a
		// rejection is made as its call is, during a test case, and named
		// against it.
		w.logger.Warn("a TLS client abandoned its handshake with keploy's proxy after the last test case was recorded: a client that does not trust keploy's CA rejects its certificate so, and so does a connection the application dialled, never used and closed at its stop (Go's http.Transport keeps such a spare), which lost nothing; no test case holds it ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.String("reason", abandonedLate.NotJudgedText()),
			zap.String("next_step", "if the client does not trust keploy's CA: "+abandonedLate.NotJudgedRemedy()+", then re-record"))
	}
	if afterLast := w.final.Since(w.charged); len(afterLast) > 0 && judge == nil {
		failed = true
		w.logger.Error("the recording is incomplete: keploy's agent could not capture every call after the last test case was recorded, and no test case holds what those calls carried ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.String("reason", afterLast.Text()),
			zap.String("next_step", "fix what the reason names, then re-record"))
	}
	if w.finalErr != nil && judge == nil {
		failed = true
		w.logger.Error("the recording is not verified: keploy could not read whether its agent captured every call after the last test case was recorded ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Error(w.finalErr),
			zap.String("next_step", "check the agent's log for why its count could not be read, then re-record"))
	}
	// Read last, after the final read: an agent started again before it
	// answered that read holding none of what the one before counted.
	var restarted string
	if w.agent != nil {
		if rep, replaced := w.agent.Replacement(); replaced && judge != nil {
			restarted = "the agent (" + rep.First + ") was started again while it recorded, as " + rep.Next + ": what it counted it could not capture, and what it captured but had not handed over, went with it"
		} else if replaced {
			failed = true
			w.logger.Error("the recording is not verified: keploy's agent was started again while it recorded, and what the agent before counted it could not capture, and what it captured but had not handed over, went with it ("+provenswitch.Env+"=1)",
				zap.String("testSetID", testSetID),
				zap.String("agentProcess", rep.First),
				zap.String("startedAgainAs", rep.Next),
				zap.String("next_step", "find why the agent restarted (its container's restart count, an OOM kill, its log), then re-record"))
		}
		w.agent.Stop()
		w.agent = nil
	}
	if len(w.unverified) > 0 && judge == nil {
		failed = true
		why := w.baseErr
		if why == nil {
			why = w.readErr
		}
		w.logger.Error("the recording is not verified: keploy could not read whether its agent captured every call while these test cases were recorded ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Strings("testCases", w.unverified),
			zap.Error(why),
			zap.String("next_step", "check the agent's log for why its count could not be read, then re-record"))
	}
	if judge == nil {
		return failed, false
	}
	l := w.recordedLocked(testSetID, realUncaptured, realUnread, restarted)
	locked = false
	w.mu.Unlock()
	if l.Failing() {
		// What fails nothing either way was said above, as it always is.
		w.logger.Warn("keploy's agent could not capture every call, or could not be checked, while this test set was recorded: the recording is kept, and the auto-replay that follows judges each of its test cases ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Strings("testCases", l.FailingTestCases()),
			zap.String("reason", l.Text()),
			zap.String("next_step", "the auto-replay's lines say which test cases replay and are kept; for one that is not, fix what the reason names (the agent's log says why a count could not be read), then re-record it"))
	}
	(*judge)(l)
	return false, l.Failing()
}

// RecordedLosses is what keploy record's capture watch found while one test
// set was recorded, under the proven switch, for a pass that judges each
// recorded test case itself (JudgeLossesLater): enterprise's auto-replay,
// which keeps only the test cases that replay. A loss in record is then
// evidence, not a verdict: a test case whose recording lost a call does not
// replay, and that test case is not kept, while the others are.
type RecordedLosses struct {
	TestSetID string
	// ByTestCase is what each test case was charged with, by its window:
	// kinds that fail a test case (judged), kinds that leave it without a
	// verdict, and evidence (a kernel's count of skipped runs) alike
	// (models.CaptureLosses.Split tells the first two apart, and takes
	// evidence into neither; models.CaptureLosses.Evidence takes it). A kind
	// that taints every later test case (not models.CaptureLossKind.PerTest)
	// is in each of them, counted from the set's start.
	ByTestCase map[string]models.CaptureLosses
	// Uncharged is what was lost after the last test case was checked,
	// beyond what the checks charged, which no test case holds: the set's.
	// Judged kinds only: a not-judged one fails nothing, and is said as the
	// set ends.
	Uncharged models.CaptureLosses
	// Unverified are the test cases whose count could not be read, and
	// UnverifiedWhy why. NotVerified is why what no test case holds could
	// not be verified (the final read failed, or the agent was started
	// again while it recorded), "" when it could.
	Unverified    []string
	UnverifiedWhy string
	NotVerified   string
	// RealUncaptured: connections to a mock.real destination keploy could
	// neither capture nor relay. RealUnread is why keploy could not read
	// whether there were any, "" when it could.
	RealUncaptured bool
	RealUnread     string
}

// Failing says l holds what fails a recording judged on its own (finish with
// no pass registered): a judged loss, a count that could not be read, or
// connections neither captured nor relayed. A pass that judges no test case
// of the set is to fail the recording on it.
func (l RecordedLosses) Failing() bool {
	if l.RealUncaptured || l.RealUnread != "" || l.NotVerified != "" || len(l.Unverified) > 0 || len(l.Uncharged) > 0 {
		return true
	}
	for _, lost := range l.ByTestCase {
		if judged, _ := lost.Split(); len(judged) > 0 {
			return true
		}
	}
	return false
}

// TestCases are the test cases l names, sorted.
func (l RecordedLosses) TestCases() []string {
	var out []string
	for n := range l.ByTestCase {
		out = append(out, n)
	}
	for _, n := range l.Unverified {
		if _, ok := l.ByTestCase[n]; !ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// FailingTestCases are the test cases l names with what fails a recording
// judged on its own: a judged loss, or a count that could not be read.
// Sorted.
func (l RecordedLosses) FailingTestCases() []string {
	var out []string
	for _, n := range l.TestCases() {
		judged, _ := l.ByTestCase[n].Split()
		if len(judged) > 0 || slices.Contains(l.Unverified, n) {
			out = append(out, n)
		}
	}
	return out
}

// Lost is everything the set's recording lost, each loss once: a test case's
// own (models.CaptureLossKind.PerTest) added up, a kind that taints every
// later test case at the most any held (each holds all of it so far), and
// what no test case holds.
func (l RecordedLosses) Lost() models.CaptureLosses {
	var own, spans models.CaptureLosses
	for _, lost := range l.ByTestCase {
		for _, c := range lost {
			if c.Kind.PerTest() {
				own = own.Plus(models.CaptureLosses{c})
			} else {
				spans = spans.Max(models.CaptureLosses{c})
			}
		}
	}
	return own.Plus(spans).Plus(l.Uncharged)
}

// Text is l in a sentence: what was lost, and what could not be read.
func (l RecordedLosses) Text() string {
	var parts []string
	judged, notJudged := l.Lost().Split()
	if t := judged.Text(); t != "" {
		parts = append(parts, t)
	}
	if t := notJudged.NotJudgedText(); t != "" {
		parts = append(parts, t)
	}
	if t := l.Lost().EvidenceText(); t != "" {
		parts = append(parts, t)
	}
	if len(l.Unverified) > 0 {
		t := fmt.Sprintf("the count could not be read for %d test case(s)", len(l.Unverified))
		if l.UnverifiedWhy != "" {
			t += " (" + l.UnverifiedWhy + ")"
		}
		parts = append(parts, t)
	}
	if l.NotVerified != "" {
		parts = append(parts, l.NotVerified)
	}
	if l.RealUncaptured {
		parts = append(parts, "keploy could neither capture nor relay connections to a mock.real destination")
	}
	if l.RealUnread != "" {
		parts = append(parts, l.RealUnread)
	}
	return strings.Join(parts, "; ")
}

// lossJudge is the pass JudgeLossesLater named, nil for none.
var lossJudge atomic.Pointer[func(RecordedLosses)]

// LossesJudgedLater says a pass registered with JudgeLossesLater judges what
// the recording loses.
func LossesJudgedLater() bool { return lossJudge.Load() != nil }

// JudgeLossesLater says that a later pass judges each test case keploy record
// records (enterprise's auto-replay, under the proven switch): from here on
// the capture watch fails no recording, and hands what it found in each test
// set, as it ends, to judge (RecordedLosses), which decides. Such a pass that
// judges none of a set's test cases (it did not run, or replayed none) is to
// fail the recording on what judge was given, when it is Failing: nothing
// else proved those test cases. undo restores record's own judging (a test's
// cleanup, a run whose auto-replay is turned off). One pass at a time: a
// second registration replaces the first, and its undo leaves none.
func JudgeLossesLater(judge func(RecordedLosses)) (undo func()) {
	lossJudge.Store(&judge)
	return func() { lossJudge.CompareAndSwap(&judge, nil) }
}

// recordedLocked is what w found in test set testSetID (RecordedLosses);
// restarted says why the agent's counts went with it, "" when they did not.
// Called with w.mu held.
func (w *captureWatch) recordedLocked(testSetID string, realUncaptured bool, realUnread, restarted string) RecordedLosses {
	l := RecordedLosses{TestSetID: testSetID, RealUncaptured: realUncaptured, RealUnread: realUnread, Unverified: slices.Clone(w.unverified)}
	if len(l.Unverified) > 0 {
		why := w.baseErr
		if why == nil {
			why = w.readErr
		}
		if why != nil {
			l.UnverifiedWhy = why.Error()
		}
	}
	if len(w.byTestCase) > 0 {
		l.ByTestCase = make(map[string]models.CaptureLosses, len(w.byTestCase))
		for n, lost := range w.byTestCase {
			l.ByTestCase[n] = slices.Clone(lost)
		}
	}
	l.Uncharged = w.final.Since(w.charged)
	var why []string
	if w.finalErr != nil {
		why = append(why, "the count could not be read after the last test case: "+w.finalErr.Error())
	}
	if restarted != "" {
		why = append(why, restarted)
	}
	l.NotVerified = strings.Join(why, "; ")
	return l
}

// String is a one-line account, for tests.
func (w *captureWatch) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return fmt.Sprintf("tainted=[%s] unverified=[%s]", strings.Join(w.tainted, ","), strings.Join(w.unverified, ","))
}
