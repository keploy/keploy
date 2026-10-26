package replay

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// PIDMapReader is an optional Instrumentation extension: the inserts into the
// agent's PID maps the kernel has refused since its hooks loaded, each of them
// a process the hooks could not track (models.PIDMapRefusals).
// *pkg/platform/http.AgentClient implements it.
//
// A process the kernel refuses a place in the agent's PID maps is not
// intercepted, so its calls are not mocked: they reach the real dependency,
// and a test whose response it shaped passes or fails against that, not
// against the recording. A test set during which the count grew is therefore
// FAILED, whatever its tests said.
type PIDMapReader interface {
	GetPIDMapRefusals(ctx context.Context) (models.PIDMapRefusals, error)
}

// pidMapReadTimeout bounds each read of the agent's refusals while the proven
// switch is off, as before it. Under the switch a read has no deadline of any
// length (design rule 15): the agent's answer, the connection's close or the
// run's own context ends it, so a slow agent is never taken for a lost count.
const pidMapReadTimeout = 10 * time.Second

// readPIDMapRefusals reads the agent's count. With the switch off it outlives
// the run's context (a stopped run still reports what was refused) under
// pidMapReadTimeout; with it on, it waits on the run's context alone.
func readPIDMapRefusals(ctx context.Context, reader PIDMapReader) (models.PIDMapRefusals, error) {
	if !provenswitch.On() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), pidMapReadTimeout)
		defer cancel()
	}
	return reader.GetPIDMapRefusals(ctx)
}

// pidMapMark is the agent's refusal count that the next test set's own are
// counted from. known is false when the agent could not say; unreadable is
// then why, for an agent that counts but whose count could not be read (nil
// for one that does not count at all: ErrPIDMapRefusalsUnsupported).
type pidMapMark struct {
	at         models.PIDMapRefusals
	known      bool
	unreadable error
}

// pidMapVerdict is what a test set's refusals say about its capture.
// incomplete is the capture-incomplete reason when the agent refused a
// process during it; unverified is the capture-unverified reason when the
// agent counts but the count could not be read, so neither a loss nor its
// absence is shown. Both are "" when nothing was refused, and for an agent
// that cannot count.
type pidMapVerdict struct {
	incomplete, unverified string
}

// markPIDMaps reads the agent's refusals as the point the next test set's
// are counted from, and the capture-loss counts with them
// (markCaptureLosses). It is called once the agent is up and before the
// application it serves starts: in Start for an agent that serves the whole
// run, and in RunTestSet for a compose agent that a test set starts afresh,
// and for a reused compose stack whose app starts for the first time; there
// the caller then moves the running application's base to it
// (appStartsAnew), but for a reused stack's app started again, whose start
// says whether it was anew. Each test set's end then moves the mark to its
// own end (pidMapCaptureIncomplete), so that every refusal of the run falls
// in exactly one test set.
func (r *Replayer) markPIDMaps(ctx context.Context) {
	r.markCaptureLosses(ctx)
	r.pidMark = pidMapMark{}
	r.pidMarked = true
	reader, ok := r.instrumentation.(PIDMapReader)
	if !ok || !r.mocksDependencies() {
		return
	}
	at, err := readPIDMapRefusals(ctx, reader)
	if err != nil {
		r.pidMark = pidMapMark{unreadable: r.pidMapReadFailed(err, false)}
		return
	}
	r.pidMark = pidMapMark{at: at, known: true}
}

// mocksDependencies reports whether this run answers dependency calls from
// the recording. With --mocking=false, or a global passthrough, every call
// goes to the real dependency by design, so a process the agent could not
// track loses nothing, and its test sets are not checked (no mark is taken).
func (r *Replayer) mocksDependencies() bool {
	return r.config.Test.Mocking && !r.config.Record.GlobalPassthrough
}

// pidMapCaptureIncomplete reads what the agent refused during the test set
// that is ending and moves the mark to now. When the agent refused a process
// it logs that at ERROR and returns why the test set's capture is incomplete.
// When the agent counts but the count could not be read -- now, or at the
// mark -- it logs that at WARN and returns why the capture is unverified: not
// a failure of the test set (no flag of keploy test asks for that), but no
// basis for deleting or mapping anything either. The next read that succeeds
// is the next mark, so a lost read costs only the test sets on either side of
// it their verdict -- the one it ends and the one it starts -- not the rest of
// the run. Nothing is returned when nothing was refused, or for an agent that
// cannot count, which would otherwise fail or hold every test set.
func (r *Replayer) pidMapCaptureIncomplete(ctx context.Context, testSetID string) pidMapVerdict {
	reader, ok := r.instrumentation.(PIDMapReader)
	if !ok || (!r.pidMark.known && r.pidMark.unreadable == nil) {
		return pidMapVerdict{}
	}
	now, err := readPIDMapRefusals(ctx, reader)
	if err != nil {
		r.pidMark = pidMapMark{unreadable: r.pidMapReadFailed(err, true)}
		if r.pidMark.unreadable == nil { // the agent never counted at all
			return pidMapVerdict{}
		}
		if ctx.Err() != nil {
			// The user stopped the run under the proven switch (its read
			// waits on the run's context alone): unverified, as any lost
			// read, but not a WARN on top of the stop.
			r.logger.Debug("the run was stopped before this test set's count was read", zap.String("test-set", testSetID), zap.Error(err))
			return pidMapVerdict{unverified: models.CaptureUnverified(r.pidMark.unreadable)}
		}
		return r.pidMapUnverified(testSetID, r.pidMark.unreadable)
	}
	mark := r.pidMark
	r.pidMark = pidMapMark{at: now, known: true}
	if !mark.known {
		// Read now, but not at the mark: what this test set refused is
		// unknown. The next one counts from here.
		return r.pidMapUnverified(testSetID, fmt.Errorf("at the start of this test set: %w", mark.unreadable))
	}
	refused := now.Since(mark.at)
	if !refused.Any() {
		return pidMapVerdict{}
	}
	reason := refused.CaptureIncomplete()
	fields := []zap.Field{
		zap.String("test-set", testSetID),
		// Refused inserts, one for every socket() of a process a map had no
		// room for: not a number of processes.
		zap.Uint64("refused_target_inserts", refused.Target),
		zap.Uint64("refused_excluded_inserts", refused.Excluded),
	}
	if refused.LastTargetTGID != 0 {
		fields = append(fields, zap.Uint32("last_untracked_tgid", refused.LastTargetTGID))
	}
	msg := "test-set failed: the agent could not track every process the application ran, so their calls were not mocked"
	if refused.Target == 0 {
		msg = "test-set failed: the agent could not exclude every process that predates the application, so their children's calls may have been mocked as the application's"
	}
	r.logger.Error(msg, append(fields, zap.String("reason", reason))...)
	return pidMapVerdict{incomplete: reason}
}

// pidMapUnverified says, at WARN, that a test set's capture could not be
// verified, and returns that as its verdict.
func (r *Replayer) pidMapUnverified(testSetID string, err error) pidMapVerdict {
	reason := models.CaptureUnverified(err)
	r.logger.Warn("this test set is not verified: keploy could not read whether the agent tracked every process the application ran, so a call may have reached the real dependency unmocked; its mocks are not pruned and mappings.yaml is not written from it",
		zap.String("test-set", testSetID), zap.Error(err), zap.String("reason", reason))
	return pidMapVerdict{unverified: reason}
}

// pidMapReadFailed notes a read of the agent's count that failed, and returns
// the failure -- nil for an agent that cannot count at all (an older agent,
// hooks without PID maps), whose test sets are left as they were. Any other
// failure is a count that was lost, which the test set's verdict says.
func (r *Replayer) pidMapReadFailed(err error, established bool) error {
	if errors.Is(err, models.ErrPIDMapRefusalsUnsupported) {
		if !established {
			// Never counted: an agent that does not count at all (an older
			// image, non-Linux hooks). Its test sets are left as decided.
			r.logger.Debug("the agent cannot say whether it tracked every process of the application; capture completeness is not checked", zap.Error(err))
			return nil
		}
		// A mark WAS read earlier, so the agent counts; a now-"unsupported"
		// answer (a 404/501) is a lost count, not "never counts".
		r.logger.Debug("the agent counted before but no longer does; treating it as an unreadable count", zap.Error(err))
		return err
	}
	r.logger.Debug("failed to read the agent's refused PID map inserts", zap.Error(err))
	return err
}

// withCaptureIncomplete puts a test set's capture-incomplete reason ahead of
// the failure reason its status gives.
func withCaptureIncomplete(captureIncomplete, reason string) string {
	switch {
	case captureIncomplete == "":
		return reason
	case reason == "":
		return captureIncomplete
	}
	return captureIncomplete + "; " + reason
}

// CaptureLossReader is an optional Instrumentation extension: every kind of
// capture loss the agent has counted since it started (models.CaptureLosses).
type CaptureLossReader interface {
	GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error)
}

// captureLossAgentReader is an optional CaptureLossReader extension: a read
// of the counts that also names the agent process that answered it
// (models.AgentInstanceHeader), "" where the answer names none (the keploy
// agent client).
type captureLossAgentReader interface {
	GetCaptureLossesAndAgent(ctx context.Context) (models.CaptureLosses, string, error)
}

// captureLossMark is, under the proven switch, the agent's capture-loss counts
// a test set's tests are counted from. known is false when they could not be
// read; err says why (models.ErrCaptureLossesUnsupported for an agent that
// cannot count, which leaves the tests as they were). agent is the agent
// process that answered the read ("" where it named none): its counts are
// its own, from its start.
type captureLossMark struct {
	at    models.CaptureLosses
	known bool
	err   error
	agent string
}

// otherAgent reports whether two reads were answered by different agent
// processes: both named one, and not the same. One that named none (an agent
// with the switch off, or older than the header) says nothing either way.
func otherAgent(a, b string) bool {
	return a != "" && b != "" && a != b
}

// noteLossAgent notes the agent process that answered a capture-loss read
// (agent; "" where it named none): the latest (lossAgent), and, where none
// was known, the one the run's bases that outlive a test set were read from
// (lossBasesAgent). A read another process answers than the one those bases
// were read from is of an agent started again in its place, or of a compose
// stack's own for the set: what the one before counted went with it, so
// those bases count nothing of this one's (lossBasesAt), until keploy starts
// the application anew under it (appStartsAnew; a compose set's start of its
// stack does, at once).
func (r *Replayer) noteLossAgent(agent string) {
	if agent == "" {
		return
	}
	r.lossAgent = agent
	if r.lossBasesAgent == "" {
		r.lossBasesAgent = agent
	}
}

// markAgentChange reads, under the proven switch as a test set begins, which
// agent process answers now. Where it is not the one the set's mark was read
// from, that one was started again since (between the sets: the set's own
// watch, which begins before this read, says whether it was during it), and
// the mark counts nothing of this one's: the set counts from this one's start
// (nil), all of what it holds being since the mark; what outlives a set
// lossBasesAt takes. An agent that cannot name its process, and a read that
// fails, leave the mark as it was (the set's own reads say the rest). Not for
// a compose stack the set brings up afresh (serveTest false): its own agent
// is not up yet, and the set marks its counts once it is (markPIDMaps).
func (r *Replayer) markAgentChange(ctx context.Context, serveTest bool) {
	if !provenswitch.On() || !r.lossMark.known || r.lossMark.agent == "" {
		return
	}
	if utils.CmdType(r.config.CommandType) == utils.DockerCompose && !serveTest {
		return
	}
	reader, ok := r.instrumentation.(captureLossAgentReader)
	if !ok {
		return
	}
	_, agent, err := reader.GetCaptureLossesAndAgent(ctx)
	if err != nil || !otherAgent(r.lossMark.agent, agent) {
		return
	}
	r.lossMark = captureLossMark{known: true, agent: agent}
	r.noteLossAgent(agent)
}

// markCaptureLosses reads, under the proven switch, the counts the next test
// set's tests are counted from: where the PID maps are marked, and again at
// each test set's end, so a loss between two test sets is the next one's.
func (r *Replayer) markCaptureLosses(ctx context.Context) {
	r.lossMark = captureLossMark{}
	r.lossMarked = true
	// A run that does not mock sends every call to the real dependency by
	// design: nothing is lost, and nothing is checked (as for the PID maps).
	// Nor does a run with no agent to ask (a --base-path run): nothing of
	// its is captured.
	if !provenswitch.On() || !r.mocksDependencies() || r.noAgent {
		r.lossMark.err = models.ErrCaptureLossesUnsupported
		return
	}
	reader, ok := r.instrumentation.(CaptureLossReader)
	if !ok {
		r.lossMark.err = models.ErrCaptureLossesUnsupported
		return
	}
	at, agent, err := readCaptureLosses(ctx, reader)
	if err != nil {
		r.lossMark.err = err
		return
	}
	r.lossMark = captureLossMark{at: at, known: true, agent: agent}
	r.noteLossAgent(agent)
	if !r.lossRunBaseSet {
		// The run's first mark, before its application starts: what keploy
		// cannot capture at all is counted from here for the whole run.
		r.lossRunBase, r.lossRunBaseSet = at, true
	}
	if !r.lossRunStartSet {
		// A process that outlives the application is counted from here.
		r.lossRunStart, r.lossRunStartSet = at, true
	}
}

// appStartsAnew is told keploy starts the application anew after the latest
// capture-loss mark (markPIDMaps, or a set that starts its own): what an
// earlier application's processes lost is not the new one's, so a process
// of the application's keploy could not place is counted from that mark on
// (lossAppBase; the hooks' load where the mark could not be read: more,
// never less). Not where the last set's stop did not end its application
// (lossAppRunsOn: a daemon the stop left running, a stop not waited out):
// that application runs on, with what it lost, and its base stays, said
// why (lossAppKept). Where another agent process answers now than the one
// the bases that outlive a test set were read from (keploy's agent was
// started again), an application started anew under the new one is all the
// new one counts: those bases are taken from it too, the run's from its start
// (nil: every count it holds is this run's, its process having started
// during it), and what keploy cannot capture at all from the mark, as at the
// run's first application. The process asked is the latest read's
// (lossAgent): the mark's own wherever the mark was read, as no read falls
// between a mark and the application's start. They differ only where the
// mark could not be read (the set's tests then cannot be counted, and say
// so): the bases are then the latest read's process's from its start, all it
// counted (more, never less), not left with a process gone, which would
// leave every later test unjudged until the application starts anew again.
func (r *Replayer) appStartsAnew() {
	if r.lossAppRunsOn {
		r.lossAppRunsOn = false
		r.lossAppKept = r.lossAppRunsOnWhy
		return
	}
	r.lossAppBase, r.lossAppKept = nil, ""
	if r.lossMark.known {
		r.lossAppBase = r.lossMark.at
	}
	if otherAgent(r.lossBasesAgent, r.lossAgent) {
		r.lossRunStart = nil
		r.lossRunBase = r.lossAppBase
		r.lossBasesAgent = r.lossAgent
	}
}

// agentsStartedBeforeApp is an optional Instrumentation extension: whether an
// agent process (models.AgentInstanceHeader) is one docker compose's
// bring-up retry started in place of keploy's agent while the application's
// container had never started (the keploy agent client's
// AgentStartedBeforeApp).
type agentsStartedBeforeApp interface {
	AgentStartedBeforeApp(instance string) bool
}

// agentStartedBeforeApp is told, under the proven switch, that a compose
// set's bring-up is done (its stack up and its application waited for,
// before its first test). Where docker compose's bring-up retry started the
// agent that answers now in place of the one before while the application's
// container had never started, the application ran against none of the
// process it replaced, and nothing of the set's was there: the set's watch
// keeps to the new one, and every count it holds is the set's and its
// application's, from its start (nil bases), the bases that outlive a set
// included. That is asked of whether the app ran against the agent replaced,
// not of where the replacement was noticed (the retry's own setup, or the
// stored-mocks check finding the mocks gone). Any other agent found replaced
// (docker or kubelet started it again under an application that ran) keeps
// the bases read from the one before, so each test is charged
// models.CaptureLossAgentStartedAgain, and the watch says the set's agent was
// started again. A read that fails leaves the bases as they are, so a test
// read from an agent started again is charged as such.
func (r *Replayer) agentStartedBeforeApp(ctx context.Context, agentWatch models.AgentInstanceWatch) {
	retry, ok := r.instrumentation.(agentsStartedBeforeApp)
	if !provenswitch.On() || !ok {
		return
	}
	if agentWatch != nil {
		if rep, replaced := agentWatch.Replacement(); replaced && retry.AgentStartedBeforeApp(rep.Next) {
			agentWatch.Accept()
		}
	}
	reader, ok := r.instrumentation.(CaptureLossReader)
	if !ok || !r.lossMarked || errors.Is(r.lossMark.err, models.ErrCaptureLossesUnsupported) {
		return
	}
	_, agent, err := readCaptureLosses(ctx, reader)
	if err != nil || !retry.AgentStartedBeforeApp(agent) {
		return
	}
	if !otherAgent(r.lossMark.agent, agent) && !otherAgent(r.lossBasesAgent, agent) {
		// The set's mark and bases are this one's already (the set's stack
		// came up after the retry and was marked once it was up): what it
		// counted before the mark, before the application started, is not
		// the set's tests'.
		return
	}
	replaced := r.lossMark.agent
	if replaced == "" || replaced == agent {
		replaced = r.lossBasesAgent
	}
	r.logger.Info("keploy's agent was started again by docker compose's bring-up retry before the application ever ran; what keploy could not capture is counted from the new agent's start",
		zap.String("agent replaced", replaced), zap.String("agent", agent))
	// No test of the set has read yet, and the set before's end cleared its
	// last read (endCaptureLosses): the set's first test counts from this
	// mark.
	r.lossMark = captureLossMark{known: true, agent: agent}
	r.lossAppBase, r.lossAppKept, r.lossRunStart, r.lossRunBase = nil, "", nil, nil
	r.lossAgent, r.lossBasesAgent = agent, agent
}

// lossBases are the bases this run's losses are counted from
// (models.LossBases): the set's mark, the running application's start, the
// run's first mark, and the run's base of what keploy cannot capture at all.
func (r *Replayer) lossBases() models.LossBases {
	return models.LossBases{Set: r.lossMark.at, App: r.lossAppBase, Run: r.lossRunStart, Capture: r.lossRunBase}
}

// lossBasesAt are the bases a read the agent process agent answered is
// counted from (lossBases), each read from another process left out: that
// one's counts went with it, and this one, started after it, counts from
// nothing (nil, its start). restarted then names the two processes, and the
// losses the one before counted after its last read here cannot be told: a
// test so counted is charged models.CaptureLossAgentStartedAgain, which
// leaves it without a verdict. The set's mark is never older than the bases
// that outlive the set, so a mark of another process comes with bases of
// another process, which restarted names. So does a read of another process
// than the read before it (the mark, or the set's last test's): the bases
// are never newer than that read either (each is the latest read's process
// as it is taken: noteLossAgent, appStartsAnew, agentStartedBeforeApp), and
// an agent process never answers again once another has, so the callers'
// checks of the read before need not name the restart themselves.
func (r *Replayer) lossBasesAt(agent string) (models.LossBases, *models.AgentReplacement) {
	b := r.lossBases()
	if otherAgent(r.lossMark.agent, agent) {
		b.Set = nil
	}
	if !otherAgent(r.lossBasesAgent, agent) {
		return b, nil
	}
	b.App, b.Run, b.Capture = nil, nil, nil
	return b, &models.AgentReplacement{First: r.lossBasesAgent, Next: agent}
}

// agentStartedAgain is the loss a test counted across an agent started again
// is charged (models.CaptureLossAgentStartedAgain), naming both processes.
func agentStartedAgain(rep models.AgentReplacement) models.CaptureLoss {
	return models.CaptureLoss{Kind: models.CaptureLossAgentStartedAgain, Count: 1,
		What: fmt.Sprintf("agent process %s, then %s", rep.First, rep.Next)}
}

// readCaptureLosses reads the agent's counts with no deadline of its own (the
// proven switch's reads: the answer, the connection's close or ctx ends it),
// and the agent process that answered, where the reader names it.
func readCaptureLosses(ctx context.Context, reader CaptureLossReader) (models.CaptureLosses, string, error) {
	if r, ok := reader.(captureLossAgentReader); ok {
		return r.GetCaptureLossesAndAgent(ctx)
	}
	losses, err := reader.GetCaptureLosses(ctx)
	return losses, "", err
}

// testCaptureLosses is, under the proven switch, what the agent could not
// capture that a test is charged, read as the test ends: processes the kernel
// refused a place in the agent's PID maps and TLS upgrades keploy could not
// make since the test set's mark -- such a loss taints the test running when
// it happened and every later test of the set (design v14 §3.2), and the
// counts only grow, so every later read carries it -- and client-side TLS
// handshakes that failed since the previous test's read (the set's mark for
// its first test): a handshake's loss is the one call it lost, the test's it
// happened in (models.CaptureLossKind.PerTest). What keploy cannot capture
// at all (decision 20) is counted from the run's base, however far back it
// was made (models.CaptureLosses.SinceWithin). A read that failed leaves the
// next test to count from the read before, so it is charged the failed
// test's handshakes too. An agent that cannot count leaves every test as it
// was, as the set's own check does; one that counts but could not be read
// now, or at the mark, is an error: the test's capture cannot be told.
func (r *Replayer) testCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	reader, ok := r.instrumentation.(CaptureLossReader)
	if !ok || errors.Is(r.lossMark.err, models.ErrCaptureLossesUnsupported) {
		return nil, nil
	}
	if !r.lossMark.known {
		if r.lossMark.err == nil {
			return nil, errors.New("keploy did not read the agent's capture-loss counts when the test set began")
		}
		return nil, fmt.Errorf("at the start of the test set: %w", r.lossMark.err)
	}
	now, agent, err := readCaptureLosses(ctx, reader)
	if err != nil {
		return nil, err
	}
	r.noteLossAgent(agent)
	from, fromAgent := r.lossMark.at, r.lossMark.agent
	if r.lossLastRead {
		from, fromAgent = r.lossLast, r.lossLastAgent
	}
	bases, restarted := r.lossBasesAt(agent)
	if otherAgent(fromAgent, agent) {
		// What this agent counted is all since the previous read: it
		// started after it. lossBasesAt has named the restart already.
		from = nil
	}
	r.lossLast, r.lossLastRead, r.lossLastAgent = now, true, agent
	charged := now.Charged(bases, from)
	if restarted != nil {
		charged = append(charged, agentStartedAgain(*restarted))
		sort.Slice(charged, func(i, j int) bool { return charged[i].Kind < charged[j].Kind })
	}
	return charged, nil
}

// captureTaintsTheSet reports whether, under the proven switch, the agent's
// counts as the set's last test ended carry a loss that taints every later
// test of the set (a kind that is not a test's own, PerTest), judged or not.
// A kernel's count of skipped runs is evidence, no verdict
// (models.CaptureLossKind.Evidence): it taints nothing, as on a kernel
// without the count.
// The counts only grow and each test is charged them from the set's mark
// (testCaptureLosses), so every test a --retry-passing-test cycle ran again
// would be charged that loss, the tests that ran clean before it included.
func (r *Replayer) captureTaintsTheSet() bool {
	if !provenswitch.On() || !r.lossMark.known || !r.lossLastRead {
		return false
	}
	bases, restarted := r.lossBasesAt(r.lossLastAgent)
	if restarted != nil {
		return true // its tests are not judged (CaptureLossAgentStartedAgain)
	}
	for _, c := range r.lossLast.SinceWithin(bases) {
		if !c.Kind.PerTest() && !c.Kind.Evidence() {
			return true
		}
	}
	return false
}

// appLossesCarriedIn is, under the proven switch, what this set's tests were
// charged, as its last test ended, of a process of the application's keploy
// could not place (models.CaptureLossKind.AppLife) that was lost before the
// set's mark: in an earlier set the application served, or as it started.
// The application ran on into this set (lossAppKept says why), and such a
// process with it. nil where there was none, and where the reads cannot be
// compared (an agent started again: its tests are not judged).
func (r *Replayer) appLossesCarriedIn() models.CaptureLosses {
	if !provenswitch.On() || !r.lossMark.known || !r.lossLastRead {
		return nil
	}
	if _, restarted := r.lossBasesAt(r.lossLastAgent); restarted != nil {
		return nil
	}
	sinceSet := map[models.CaptureLossKind]uint64{}
	for _, c := range r.lossLast.Since(r.lossMark.at) {
		sinceSet[c.Kind] = c.Count
	}
	var out models.CaptureLosses
	for _, c := range r.lossLast.Since(r.lossAppBase) {
		if c.Kind.AppLife() && c.Count > sinceSet[c.Kind] {
			out = append(out, models.CaptureLoss{Kind: c.Kind, Count: c.Count - sinceSet[c.Kind], What: c.What})
		}
	}
	return out
}

// appLossesCarriedInReason is a set's reason for the losses its tests were
// charged from before it began (appLossesCarriedIn): what they were, and why
// the application they belong to ran on into the set (kept).
func appLossesCarriedInReason(carried models.CaptureLosses, kept string) string {
	if kept == "" {
		kept = "the application ran on from before this test-set"
	}
	return "capture incomplete: its tests are charged what keploy could not capture of the application's processes before this test-set began, as such a process runs on with the application, and " + kept + ": " +
		strings.TrimPrefix(carried.Text(), "capture incomplete: ") +
		"; the earlier test-set's report says where it was lost, and each test charged with it says what to do"
}

// endCaptureLosses reads, at a test set's end under the proven switch, what
// the agent lost after the set's last per-test read (after its mark, when no
// test read), and moves the mark to now for the next test set; an
// application keploy then starts anew counts from it too (appStartsAnew).
// Nothing for an agent that cannot count or a run that does not mock. An
// error is a read that failed: what the set's tail lost cannot be told, and
// the next set's tests cannot be counted.
func (r *Replayer) endCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	reader, ok := r.instrumentation.(CaptureLossReader)
	mark, last, lastRead, lastAgent := r.lossMark, r.lossLast, r.lossLastRead, r.lossLastAgent
	r.lossLast, r.lossLastRead, r.lossLastAgent = nil, false, ""
	if !provenswitch.On() || !ok || !r.mocksDependencies() || r.noAgent || errors.Is(mark.err, models.ErrCaptureLossesUnsupported) {
		r.markCaptureLosses(ctx)
		return nil, nil
	}
	now, agent, err := readCaptureLosses(ctx, reader)
	r.lossMarked = true
	if err != nil {
		r.lossMark = captureLossMark{err: err}
		return nil, err
	}
	r.lossMark = captureLossMark{at: now, known: true, agent: agent}
	r.noteLossAgent(agent)
	if !mark.known {
		return nil, nil // the set's tests were unjudged for it already
	}
	from, fromAgent := mark.at, mark.agent
	if lastRead {
		from, fromAgent = last, lastAgent
	}
	// What keploy cannot capture at all over a connection that outlives
	// tests: only what is new since the set's last read (its tests already
	// carry the rest), counted from the run's base when no test read. A
	// standing unix_unseen is always new (no mark takes it away): such a set
	// is not judged. Every other kind, an abandoned TLS handshake too, from
	// the set's last read or its mark (SinceWithin).
	notFrom, notFromAgent := r.lossRunBase, r.lossBasesAgent
	if lastRead {
		notFrom, notFromAgent = last, lastAgent
	}
	// A base another agent process answered counts nothing of this one's,
	// which started after it: all this one counted is since (nil), and what
	// the one before counted after its last read cannot be told. notFrom's
	// agent is fromAgent where a test read, and the bases' (no newer than
	// the mark's: lossBasesAt) where none did, so a from of another process
	// comes with a notFrom of another process, which names the restart.
	var restarted *models.AgentReplacement
	if otherAgent(fromAgent, agent) {
		from = nil
	}
	if otherAgent(notFromAgent, agent) {
		notFrom, restarted = nil, &models.AgentReplacement{First: notFromAgent, Next: agent}
	}
	// Not the PID-map refusals: the set's PID-map check
	// (pidMapCaptureIncomplete) reports those, once.
	var out models.CaptureLosses
	for _, l := range now.SinceWithin(models.LossBases{Set: from, App: from, Run: from, Capture: notFrom}) {
		if l.Kind != models.CaptureLossPIDMap && l.Kind != models.CaptureLossPIDMapExcluded {
			out = append(out, l)
		}
	}
	if restarted != nil {
		out = append(out, agentStartedAgain(*restarted))
		sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	}
	return out, nil
}
