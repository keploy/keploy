package record

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

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
// unverified, and fails the recording too. With the switch off, or an agent
// that cannot count, it does nothing.
type captureWatch struct {
	logger *zap.Logger
	reader CaptureLossReader
	off    bool
	base   models.CaptureLosses
	// last is the count as the previous test case was checked, which a test
	// case's own kinds (models.PerTest) count from; nil before the first.
	last models.CaptureLosses
	// baseErr is why the count could not be read when capture began; every
	// test case is then unverified.
	baseErr error

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
	// chargedNotJudged and finalNotJudged are charged's and final's
	// counterparts for what leaves a test case without a verdict: an
	// abandoned TLS handshake beyond the most the checks saw was after the
	// last test case, said (a Unix socket's is every test case's from the
	// first on already).
	chargedNotJudged models.CaptureLosses
	finalNotJudged   models.CaptureLosses
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
		return w
	}
	w.reader = reader
	// Begun before the count is read, so the agent that answers it is the
	// one the recording's checks keep to.
	if a, ok := instr.(agentInstanceWatcher); ok {
		w.agent = a.WatchAgentInstance()
	}
	// No deadline of its own (rule 15): the agent's answer, or the
	// connection's close, ends it. Not the stop: from here a stop drains what
	// the agent captured (Start), and each test case it saves is checked
	// against this count, so a stop that cut the read short would fail the
	// recording the user stopped as "not verified". A second interrupt, which
	// gives the drain up, ends it.
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRead()
	go func() {
		select {
		case <-utils.StopNow():
			cancelRead()
		case <-readCtx.Done():
		}
	}()
	base, err := reader.GetCaptureLosses(readCtx)
	switch {
	case errors.Is(err, models.ErrCaptureLossesUnsupported):
		logger.Debug("the agent cannot say whether it captured every call; capture completeness is not checked", zap.Error(err))
		w.off = true
		if w.agent != nil {
			w.agent.Stop()
			w.agent = nil
		}
	case err != nil:
		w.baseErr = err
	default:
		w.base = base.Without(models.CaptureLossContainerUnread)
	}
	return w
}

// afterTestCase checks the count as test case name is saved: an agent-life
// loss since the recording began taints it, and a failed TLS handshake since
// the previous check (models.PerTest) is its own. A check that failed leaves
// the next one to count from the check before.
func (w *captureWatch) afterTestCase(ctx context.Context, name string) {
	if w == nil || w.off {
		return
	}
	if w.baseErr != nil {
		w.mu.Lock()
		w.unverified = append(w.unverified, name)
		w.mu.Unlock()
		return
	}
	now, err := w.reader.GetCaptureLosses(context.WithoutCancel(ctx))
	now = now.Without(models.CaptureLossContainerUnread) // charged by its mock (mapped)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.unverified = append(w.unverified, name)
		if w.readErr == nil {
			w.readErr = err
		}
		return
	}
	test := w.last
	if test == nil {
		test = w.base
	}
	w.last = now
	judged, notJudged := now.Charged(w.base, w.base, test).Split()
	// Every loss so far: the reason names them all, not only this test
	// case's own (an earlier one's failed handshake is still a reason the
	// recording is incomplete), and each was charged to a test case by now.
	allJudged, allNotJudged := now.Since(w.base).Split()
	w.charged = w.charged.Max(allJudged)
	w.chargedNotJudged = w.chargedNotJudged.Max(allNotJudged)
	if len(judged) > 0 {
		w.tainted = append(w.tainted, name)
		w.lost = allJudged
	}
	if len(notJudged) > 0 {
		w.uncapturable = append(w.uncapturable, name)
		w.notJudged = allNotJudged
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
		w.unreadLost = w.unreadLost.Plus(models.CaptureLosses{{Kind: models.CaptureLossContainerUnread, Count: 1, What: what}})
	}
}

// afterTestCases is the final read, once the app is stopped and before the
// agent may stop: what was lost after the last test case was checked is
// charged to no test case, so no test case's mocks miss it, and the
// recording fails on it (design v14 §7.5: evidence charged to no test).
// Without it a loss after the last test case went unsaid.
func (w *captureWatch) afterTestCases(ctx context.Context) {
	if w == nil || w.off || w.baseErr != nil {
		return
	}
	// No deadline of its own (rule 15), as the base read: the agent's
	// answer ends it, or the second interrupt, which gives the stop's waits
	// up (the read is then "not verified", and the recording fails).
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRead()
	go func() {
		select {
		case <-utils.StopNow():
			cancelRead()
		case <-readCtx.Done():
		}
	}()
	now, err := w.reader.GetCaptureLosses(readCtx)
	now = now.Without(models.CaptureLossContainerUnread) // charged by its mock (mapped)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.finalErr = err
		return
	}
	w.final, w.finalNotJudged = now.Since(w.base).Split()
}

// finish says, at ERROR, which test cases were recorded while the agent could
// not capture every call, or could not be checked, and reports whether the
// recording is to fail for it.
func (w *captureWatch) finish(testSetID string) bool {
	if w == nil || w.off {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	failed := false
	if len(w.tainted) > 0 {
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
		w.logger.Warn("these test cases were recorded while or after the application called what keploy cannot capture, while a TLS client abandoned its handshake, or while the Docker daemon did not answer keploy's read of a container they called: such calls are not in the mocks, or not as that container's ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Strings("testCases", w.uncapturable),
			zap.String("reason", notJudged.NotJudgedText()),
			zap.String("next_step", notJudged.NotJudgedRemedy()+", then re-record these test cases"))
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
	if afterLast := w.final.Since(w.charged); len(afterLast) > 0 {
		failed = true
		w.logger.Error("the recording is incomplete: keploy's agent could not capture every call after the last test case was recorded, and no test case holds what those calls carried ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.String("reason", afterLast.Text()),
			zap.String("next_step", "fix what the reason names, then re-record"))
	}
	if w.finalErr != nil {
		failed = true
		w.logger.Error("the recording is not verified: keploy could not read whether its agent captured every call after the last test case was recorded ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Error(w.finalErr),
			zap.String("next_step", "check the agent's log for why its count could not be read, then re-record"))
	}
	// Read last, after the final read: an agent started again before it
	// answered that read holding none of what the one before counted.
	if w.agent != nil {
		if rep, replaced := w.agent.Replacement(); replaced {
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
	if len(w.unverified) > 0 {
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
	return failed
}

// String is a one-line account, for tests.
func (w *captureWatch) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return fmt.Sprintf("tainted=[%s] unverified=[%s]", strings.Join(w.tainted, ","), strings.Join(w.unverified, ","))
}
