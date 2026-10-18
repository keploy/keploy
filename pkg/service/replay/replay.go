package replay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"facette.io/natsort"
	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	grpcMatcher "go.keploy.io/server/v3/pkg/matcher/grpc"
	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/coverage"
	"go.keploy.io/server/v3/pkg/platform/coverage/golang"
	"go.keploy.io/server/v3/pkg/platform/coverage/java"
	"go.keploy.io/server/v3/pkg/platform/coverage/javascript"
	"go.keploy.io/server/v3/pkg/platform/coverage/python"
	"go.keploy.io/server/v3/pkg/platform/telemetry"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/realclient"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const applicationFailedToRunLogMessage = "application failed to run; check the application logs for details or verify the app command is correct"

// startupMockCutoff returns the startup-mock exemption boundary for a test set:
// any mock recorded before this time is a startup mock that UpdateMocks must
// keep even when unconsumed. It is the request timestamp of the
// (models.StartupMockTestCaseWindow+1)-th test case (ordered by request time),
// so the startup window covers app boot through the recording of the Nth test
// case — the replay-side twin of the record-side IsStartup tagging keyed off
// SyncMockManager.resolvedTestCount.
//
// When the set has <= models.StartupMockTestCaseWindow test cases, the entire
// recording is startup; keepAll (the replay start time, pruneBefore) is
// returned so every recorded mock is retained — mocks recorded before replay
// start are kept by this exemption and any written after it are kept by
// UpdateMocks' post-replay-write rule. Returns the zero time when no test case
// carries a usable timestamp, which disables the exemption (matching the prior
// behaviour for timestamp-less sets).
func startupMockCutoff(testCases []*models.TestCase, keepAll time.Time) time.Time {
	tcTimes := make([]time.Time, 0, len(testCases))
	for _, tc := range testCases {
		var candidate time.Time

		// Prefer high-precision request timestamps when available.
		if !tc.HTTPReq.Timestamp.IsZero() {
			candidate = tc.HTTPReq.Timestamp
		} else if !tc.GrpcReq.Timestamp.IsZero() {
			candidate = tc.GrpcReq.Timestamp
		} else if tc.Created > 0 {
			// Fallback to the coarser Created timestamp.
			candidate = time.Unix(tc.Created, 0)
		}

		if !candidate.IsZero() {
			tcTimes = append(tcTimes, candidate)
		}
	}
	sort.Slice(tcTimes, func(i, j int) bool { return tcTimes[i].Before(tcTimes[j]) })

	if len(tcTimes) > models.StartupMockTestCaseWindow {
		return tcTimes[models.StartupMockTestCaseWindow]
	}
	if len(tcTimes) > 0 {
		return keepAll
	}
	return time.Time{}
}

// abortAfterTestSet reports whether the run stops after a test set that ended
// with status. A set that is only not judged (its evidence could not all be
// read: notJudged) does not stop it: the set fails as not judged, with every
// reason, and the run goes on so every set gets its verdict; the run exits
// non-zero. Anything else that is INTERNAL_ERR ends the round
// (shouldAbortTestRun).
func abortAfterTestSet(status models.TestSetStatus, cmdType utils.CmdType, notJudged bool) bool {
	if notJudged && status == models.TestSetStatusInternalErr {
		return false
	}
	return shouldAbortTestRun(status, cmdType)
}

func shouldAbortTestRun(status models.TestSetStatus, cmdType utils.CmdType) bool {
	switch status {
	case models.TestSetStatusAppHalted, models.TestSetStatusFaultUserApp:
		return cmdType != utils.DockerCompose
	case models.TestSetStatusInternalErr:
		return true
	default:
		return false
	}
}

func mapAppErrorToTestSetStatus(appErr models.AppError) (models.TestSetStatus, bool) {
	switch appErr.AppErrorType {
	case "":
		return models.TestSetStatusAppHalted, true
	case models.ErrCtxCanceled:
		return "", false
	case models.ErrCommandError:
		return models.TestSetStatusFaultUserApp, true
	case models.ErrUnExpected, models.ErrAppStopped, models.ErrTestBinStopped:
		return models.TestSetStatusAppHalted, true
	case models.ErrInternal:
		return models.TestSetStatusInternalErr, true
	default:
		return models.TestSetStatusAppHalted, true
	}
}

func resolveTestSetStatus(cmdType utils.CmdType, current, derived models.TestSetStatus, err error) (models.TestSetStatus, bool) {
	switch current {
	case models.TestSetStatusAppHalted, models.TestSetStatusFaultUserApp, models.TestSetStatusInternalErr, models.TestSetStatusUserAbort:
		return current, true
	}
	switch derived {
	case models.TestSetStatusAppHalted, models.TestSetStatusFaultUserApp, models.TestSetStatusInternalErr, models.TestSetStatusUserAbort:
		return derived, true
	}
	if cmdType == utils.DockerCompose && isDockerComposeReplayShutdown(err) {
		return models.TestSetStatusAppHalted, true
	}
	return "", false
}

func isDockerComposeReplayShutdown(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "connection reset by peer") ||
		strings.Contains(errMsg, "broken pipe") ||
		strings.Contains(errMsg, "server closed") ||
		containsStandalonePhrase(errMsg, "unexpected eof") ||
		containsStandalonePhrase(errMsg, "eof") ||
		strings.Contains(errMsg, "found no test results")
}

func containsStandalonePhrase(msg, phrase string) bool {
	start := 0
	for {
		idx := strings.Index(msg[start:], phrase)
		if idx == -1 {
			return false
		}
		idx += start

		beforeIdx := idx - 1
		afterIdx := idx + len(phrase)
		beforeOK := beforeIdx < 0 || !isWordChar(msg[beforeIdx])
		afterOK := afterIdx >= len(msg) || !isWordChar(msg[afterIdx])
		if beforeOK && afterOK {
			return true
		}

		start = idx + 1
	}
}

func isWordChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_'
}

// runShape carries what the report's reason string needs to know beyond the
// status. Two separate facts, because they are not interchangeable: a run can
// stop early for a reason that ALREADY has an honest diagnosis (an app that
// crashed), and that diagnosis must survive.
type runShape struct {
	// stoppedEarly: the cycle ended before every test it intended to run had
	// produced a verdict.
	stoppedEarly bool
	// downgradedFromPassed: stoppedEarly is what turned an otherwise-PASSED set
	// into APP_FAULT, so the status describes the partial run itself rather
	// than something that went wrong independently.
	downgradedFromPassed bool
	// notJudged is why an otherwise judged set is INTERNAL_ERR: evidence its
	// verdict needs could not be read (proven switch). Empty otherwise.
	notJudged string
	// realWhy is why mock.real fails the set (realSetFailure): a destination
	// its recordings list that its run never reached, or that never
	// answered. Empty otherwise.
	realWhy string
	// unclaimedWhy is why the calls the application made after the set's
	// last test that no mock answered fail it (unclaimedCalls). Empty
	// otherwise.
	unclaimedWhy string
	// staleWhy names the tests of a set with a verdict of its own (FAILED,
	// a partial run's APP_FAULT) that are not judged for a stale mapping
	// (FailureInfo.MappingStale). Empty otherwise: a set not judged says it
	// in notJudged.
	staleWhy string
}

const partialRunReason = "replay stopped before every test ran - the tests missing from the totals were not " +
	"verified; check the app_logs field and keploy's own logs for why the run ended early"

// withUnverifiedNote appends the unverified-tests note to a diagnosis that
// stands on its own, so a reader gets both facts rather than whichever one the
// status happened to name.
func withUnverifiedNote(base string, shape runShape) string {
	if !shape.stoppedEarly {
		return base
	}
	return base + "; it also stopped before every test ran, so treat the tests missing from the totals as unverified"
}

func describeTestSetFailure(status models.TestSetStatus, testCaseResults []models.TestResult, shape runShape) string {
	reason := describeTestSetVerdict(status, testCaseResults, shape)
	switch {
	case shape.staleWhy == "":
		return reason
	case reason == "":
		return shape.staleWhy
	}
	return reason + "; " + shape.staleWhy
}

// describeTestSetVerdict is describeTestSetFailure less the tests a set with a
// verdict of its own does not judge for a stale mapping (runShape.staleWhy).
func describeTestSetVerdict(status models.TestSetStatus, testCaseResults []models.TestResult, shape runShape) string {
	switch status {
	case models.TestSetStatusAppHalted, models.TestSetStatusFaultUserApp:
		// A run that stopped part-way gets its own wording, but ONLY when this
		// status came from that fact. It reaches this status without the app
		// necessarily having stopped — the common cause is the agent being
		// replaced mid-set and then refusing the per-test filter params — so
		// "application stopped during replay" would send the reader to an
		// application that is still running and still healthy.
		//
		// The inverse matters just as much: an app that genuinely crashed
		// mid-set ALSO stopped the run early, and it must keep its own
		// diagnosis rather than be told to go and read keploy's logs.
		if shape.downgradedFromPassed {
			return partialRunReason
		}
		if len(testCaseResults) == 0 {
			return "application startup failed - check application logs in the app_logs field for details and next steps"
		}
		return withUnverifiedNote(
			"application stopped during replay - check application logs in the app_logs field for details and next steps",
			shape)
	case models.TestSetStatusFailed:
		// A set that both failed a test AND stopped early said nothing at all
		// about the second half: the report read `Total 4, passed 0, failed 1`
		// with an empty reason, so the natural reading was that the other three
		// were skipped for some legitimate purpose. That is the same misreading
		// as #4618, arriving on a red set instead of a green one.
		if shape.stoppedEarly {
			return partialRunReason
		}
		if shape.realWhy != "" && shape.unclaimedWhy != "" {
			return shape.realWhy + "; " + shape.unclaimedWhy
		}
		return shape.realWhy + shape.unclaimedWhy
	case models.TestSetStatusInternalErr:
		if shape.notJudged != "" {
			return withUnverifiedNote(shape.notJudged, shape)
		}
		return withUnverifiedNote("replay failed with an internal error - please report this issue if it persists", shape)
	default:
		return ""
	}
}

// scoredNothing reports whether a test-set loaded test cases and produced no
// outcome for a single one of them: nothing passed, nothing failed, nothing was
// ignored, nothing was marked obsolete.
//
// testSetStatus starts at PASSED and is only ever downgraded, so such a run
// reaches the report still PASSED and is published as a green suite. Observed
// on enterprise pipeline 9321: a dependency crashed the compose stack, keploy's
// bring-up retry replaced the agent container so its mocks were gone, every
// request timed out against the readiness ceiling, and the run printed
// "Total tests: 4, passed 0, failed 0" and exited 0. Nothing was verified, and
// nothing said so.
//
// All FOUR counters, because each of the other three means the tests did run:
//   - ignored: the user asked for those to be skipped.
//   - obsolete: with StrictFailure off, a mock-set mismatch demotes a test that
//     executed and answered. An all-obsolete set is drifted mocks, not a dead
//     app — and calling it APP_FAULT would abort the remaining test-sets on the
//     native and docker-run paths (shouldAbortTestRun), turning a re-record
//     nudge into a halted run.
//
// A set with no test cases at all is NOT this: it returns earlier as
// NO_TESTS_TO_RUN, which is an honest answer to an honest question.
func scoredNothing(status models.TestSetStatus, loaded, success, failure, ignored, obsolete int) bool {
	return status == models.TestSetStatusPassed && loaded > 0 &&
		success == 0 && failure == 0 && ignored == 0 && obsolete == 0
}

// rewindConsumedForRetryCycle folds the finished cycle's consumption into the
// across-cycles record and returns the map the next cycle starts from: the
// baseline, so per-test single-use mocks are servable again.
//
// It also marks the AGENT's own consumption history stale. That rewind reaches
// the CLI's map only; under KEPLOY_AGENT_OWNS_CONSUMED the agent filters from
// its own history and nothing rewinds that one, so the retry would filter
// against the previous cycle and fail with match_phase=no_mocks — precisely the
// failure the rewind exists to prevent (#4622).
func (r *Replayer) rewindConsumedForRetryCycle(total, baseline, allAcrossCycles map[string]models.MockState) map[string]models.MockState {
	for k, v := range total {
		allAcrossCycles[k] = v
	}
	rewound := make(map[string]models.MockState, len(baseline))
	for k, v := range baseline {
		rewound[k] = v
	}
	r.agentHistoryStaleForSet.Store(true)
	return rewound
}

// agentConsumedHistoryUnusable reports whether the agent's own consumption
// history can still be trusted for this send. Either cause disqualifies it.
func (r *Replayer) agentConsumedHistoryUnusable() bool {
	return r.agentHistoryIncompleteForRun.Load() || r.agentHistoryStaleForSet.Load()
}

// mockOutgoingForTestSet issues the per-test-set MockOutgoing and, only once
// the agent has ANSWERED it, drops the retry-rewind staleness.
//
// MockOutgoing is a synchronous call, and a nil error means the agent's handler
// ran to completion. That handler reaches Proxy.Mock, which either installs a
// brand-new MockManager (the first Mock of an agent process — its
// consumedPersistent starts empty) or calls ResetForReplaySession on the
// existing one, which wipes it. Either way the map the agent filters from
// under KEPLOY_AGENT_OWNS_CONSUMED is empty on return, so the history a rewind
// made stale is provably gone and the next set starts with both sides empty.
// pkg/agent/proxy pins that Mock really does this; it is one deletable line.
//
// The invariant is cross-process and not negotiated: a nil error proves the
// agent we CONTACTED reset, not that it is new enough to do so. An agent image
// older than the per-name clear has that bug independently of this flag — it is
// #4620, fixed agent-side — but be aware this stops masking it after a rewind.
//
// The ordering IS the safety argument, and it is asymmetric. Clearing late
// only keeps paying the marshaling this flag exists to avoid. Clearing early
// hands the new set's pool to an agent still holding the previous set's
// history — and mock names are unique only WITHIN a set, so the old mock-1
// (Usage: Deleted) would drop this set's unrelated mock-1 and stamp its
// IsFiltered/SortOrder onto it. That is a wrong test result, and it is exactly
// the defect #4621 fixed, re-introduced from the CLI side. Never hoist this
// above the call.
//
// Deliberately not used by the mid-set repair in ensureAgentHoldsStoredMocks:
// that MockOutgoing is a repair, not a set boundary, and its own latch must
// stay set across it.
func (r *Replayer) mockOutgoingForTestSet(ctx context.Context, opts models.OutgoingOptions) error {
	// Under the proven switch the agent is handed mock.real before each set
	// (design v14 §4): its destinations are reached for real. Only the run's
	// first push starts the agent's REAL bookkeeping afresh: what came
	// between two sets (a pooled connection's bytes, a connection keploy
	// could not capture) belongs to the run.
	if !r.noAgent {
		push := realclient.Push
		if r.realPushed.Load() {
			// The same run's next set: an agent that holds the list keeps
			// what the run saw; a replacement (a compose stack restarted
			// per set) is handed it afresh.
			push = realclient.Keep
		}
		if err := push(ctx, r.logger, r.instrumentation, r.mockReal()); err != nil {
			return err
		}
		r.realPushed.Store(true)
	}
	// A set boundary, unlike the repair's re-registration: what the agent
	// recorded consumed and nobody drained is the previous set's, which the
	// set's staging drops (models.OutgoingOptions.StartsTestSet).
	opts.StartsTestSet = true
	if err := r.instrumentation.MockOutgoing(ctx, opts); err != nil {
		return err
	}
	r.agentHistoryStaleForSet.Store(false)
	return nil
}

// cycleIncomplete reports that a retry cycle ended before every test it
// intended to run had produced a verdict.
//
// It compares against the cycle's OWN testsToRun rather than the loaded count,
// which is the only comparison that survives the shapes around it: `success` is
// ASSIGNED per cycle while `failure`/`obsolete` accumulate across cycles, and
// the loaded count also covers the ignored, the deferred streaming tests and
// anything a selected-test list dropped.
//
// Every test that RUNS lands in exactly one of passed/obsolete/failed, so a
// shortfall means the loop broke out early — unless the loop deliberately
// passed a test over, which records no verdict and is not a fault. That is what
// `skipped` carries. Both in-loop skip guards (a test absent from the selected
// list, and one in the ignored list) duplicate the pre-loop filter that builds
// activeTestCases, and testsToRun only ever holds tests that already cleared
// it, so today neither can fire and skipped is always 0.
//
// Counting them anyway is deliberate: it costs nothing and stops this
// predicate resting on that reachability argument. The asymmetry is the reason
// — a missed skip reports a healthy run as an app fault, and APP_FAULT aborts
// every remaining test-set on the native and docker-run paths
// (shouldAbortTestRun). Being wrong in that direction is worse than the bug
// this function exists to catch.
func cycleIncomplete(success, failure, obsolete, skipped, intended int) bool {
	return success+failure+obsolete+skipped < intended
}

func shouldIncludeAppLogs(status models.TestSetStatus) bool {
	switch status {
	case models.TestSetStatusAppHalted, models.TestSetStatusFaultUserApp, models.TestSetStatusInternalErr:
		return true
	default:
		return false
	}
}

type Replayer struct {
	// notJudgedSets are the test sets of this run that ended not judged
	// (markNotJudged): INTERNAL_ERR that does not end the round.
	notJudgedMu   sync.Mutex
	notJudgedSets map[string]bool
	// neverReadySets are those of them not judged because their application
	// never got ready (proven switch): they ran no test, and have no results.
	neverReadySets map[string]bool

	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	mappingDB       MappingDB
	reportDB        ReportDB
	testSetConf     TestSetConfig
	telemetry       Telemetry
	instrumentation Instrumentation
	config          *config.Config
	instrument      bool
	noAgent         bool // runsWithoutAgent, decided once in NewReplayer and handed to its hooks
	// realPushed: this run's agent holds its mock.real (mockOutgoingForTestSet).
	realPushed     atomic.Bool
	isLastTestSet  bool
	isLastTestCase bool
	// finalAttempt: the set's attempt now running is the last one the run
	// can give it (MaxFlakyChecks); no attempt of it comes after.
	finalAttempt bool
	// keepAliveStop, under the proven switch, stops the application
	// --keep-app-alive started once for the run (natively, or with docker
	// run), waits for it, and says why it had to kill it, "" when it did not
	// (P101-c). The last set's final attempt calls it before its end's read.
	// nil otherwise.
	keepAliveStop func() string

	// exact is the set being replayed's exact serving, under the proven
	// switch (exact.go); nil otherwise.
	exact *exactPlan
	// ignoredSaid holds the settings said ignored under the switch
	// (noteIgnored), each once per run.
	ignoredSaid sync.Map

	// readySince is the agent's count of listens read before this set's
	// application was started anew (markReadiness), under the proven switch;
	// nil when none was read, readySinceErr when it could not be.
	readySince    *readyMark
	readySinceErr string
	// readyStalledAt is where the last set's wait called the application
	// stalled; nil once it got ready.
	readyStalledAt *progressMark
	// badHealthURLSaid says an invalid --health-url once a run.
	badHealthURLSaid sync.Once
	// readyTally is provenReadiness.tally for the sets' waits: tests only.
	readyTally *readTally

	// isFirstTestSet mirrors isLastTestSet — true while the test-set
	// about to run is the first one of the current replay run. Read
	// by the waitForAppReady gate so --delay applies only to the first
	// test-set when --keep-app-alive is set (subsequent test-sets
	// inherit a warm app and would otherwise pay --delay seconds of
	// dead time per boundary).
	isFirstTestSet     bool
	runDomainSet       *telemetry.DomainSet // collects host domains across a test run for telemetry
	testRunTestSets    []string             // all test set IDs for the current run (used by RunTestSet)
	testRunID          string               // current test run ID (used by RunTestSet)
	afterTestRunCalled bool                 // guards duplicate AfterTestRun calls
	hookImpl           TestHooks
	// The agent's own consumption history stops being authoritative for two
	// different reasons with two different lifetimes, so they are two latches.
	// Either one makes a filter-params send carry the CLI's map instead
	// (KEPLOY_AGENT_OWNS_CONSUMED is ignored for that send).
	//
	// agentHistoryIncompleteForRun: a replacement agent was re-registered
	// mid-set, so everything consumed before it started is missing from its
	// record and cannot be recovered WITHIN that set.
	//
	// Kept for the whole run, which is conservative rather than necessary: by
	// the same argument that expires the set-scoped latch below, the next
	// boundary wipes the replacement's history and RunTestSet allocates a fresh
	// CLI map, so both sides are empty and back in agreement. Un-sticking it
	// would buy the optimization back only for the remainder of a run in which
	// a repair already fired — an already-degraded path — at the cost of a
	// second piece of reasoning about a process we just concluded we could not
	// trust. The cost of leaving it is marshaling, and the failure direction is
	// the always-correct legacy path.
	agentHistoryIncompleteForRun atomic.Bool
	// agentHistoryStaleForSet: a --retry-passing-test cycle rewound the CLI's
	// map to its baseline so single-use mocks are servable again. Nothing
	// rewinds the agent's, so within this set the retry would be filtered
	// against the previous cycle and fail with match_phase=no_mocks.
	//
	// Scoped to the SET, because this cause expires at the boundary: #4621
	// makes the agent wipe its per-name history in ResetForReplaySession, which
	// the per-set MockOutgoing triggers, so from there on the rewind has
	// nothing left to be stale against. Cleared ONLY by mockOutgoingForTestSet,
	// i.e. strictly after the agent has answered that call.
	agentHistoryStaleForSet atomic.Bool

	completeTestReport    map[string]TestReportVerdict
	firstRun              bool
	completeTestReportMu  sync.RWMutex
	totalTests            int
	totalTestPassed       int
	totalTestFailed       int
	totalTestObsolete     int
	totalTestIgnored      int
	consumedMockNames     map[string]struct{} // distinct mock names consumed across all test sets in the current run; size emitted as Mocks-Consumed on the TestRun event
	totalTestTimeTaken    time.Duration
	failedTCsBySetID      map[string][]string
	mockMismatchFailures  *TestFailureStore
	fallbackDeprecateOnce sync.Once
	// quarantined is, under the proven switch, each test this run skipped for
	// a reason, keyed by test set and test, for the run's summary.
	quarantinedMu sync.Mutex
	quarantined   map[[2]string]string
	// pidMark is where the next test set's refused PID map inserts are
	// counted from (pidmaps.go).
	pidMark pidMapMark
	// pidMarked is whether this run has taken a mark yet: a compose stack
	// reused across test sets is marked by the first test set that reaches
	// its agent, whichever that is (a first test set can end before it).
	pidMarked bool
	// lossMark is, under the proven switch, where the next test set's tests
	// count the agent's capture losses from (pidmaps.go); lossMarked is
	// whether this run has taken one yet; lossLast is the latest per-test
	// read, which the set's end counts what is left from.
	lossMark     captureLossMark
	lossMarked   bool
	lossLast     models.CaptureLosses
	lossLastRead bool
	// lossRunBase is the run's first capture-loss mark, from which what
	// keploy cannot capture at all is counted for the whole run (decision
	// 20: a Unix-socket connection outlives the test set it was opened in).
	lossRunBase    models.CaptureLosses
	lossRunBaseSet bool
	// setEnd is how the final report of the test set RunTestSet is running
	// ends it, once it has one (noteSetEnd): the status it says, and why
	// (tellSetEnd). Reset before each run of a set.
	setEnd *models.SetEnd
	// setCalls are the calls of the set RunTestSet is running that no mock
	// answered and that are the set's, no test's (proven switch): those made
	// before its first test (before_set_calls.go).
	setCalls setCalls
}

// GetMockMismatchFailures returns a copy of the accumulated mock mismatch failures.
func (r *Replayer) GetMockMismatchFailures() []TestFailure {
	return r.mockMismatchFailures.GetFailures()
}

// GetTestRunID returns the current test run ID.
func (r *Replayer) GetTestRunID() string {
	return r.testRunID
}

func NewReplayer(logger *zap.Logger, testDB TestDB, mockDB MockDB, reportDB ReportDB, mappingDB MappingDB, testSetConf TestSetConfig, telemetry Telemetry, instrumentation Instrumentation, storage Storage, config *config.Config) Service {
	instrument := config.Command != ""
	// Decided here, from the config as it is now, and not read again: a caller
	// may change the command after this (enterprise's cloud replay builds the
	// replayer under a placeholder command and then puts the user's back).
	noAgent := runsWithoutAgent(config)
	defaultHook := newHooks(logger, config, instrumentation, noAgent)

	return &Replayer{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		mappingDB:       mappingDB,
		reportDB:        reportDB,
		testSetConf:     testSetConf,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          config,
		instrument:      instrument,
		noAgent:         noAgent,
		hookImpl:        defaultHook,

		completeTestReport:   make(map[string]TestReportVerdict),
		failedTCsBySetID:     make(map[string][]string),
		mockMismatchFailures: NewTestFailureStore(),
	}
}

func (r *Replayer) SetTestHooks(testHooks TestHooks) {
	if testHooks == nil {
		return
	}
	r.hookImpl = testHooks
}

func (r *Replayer) GetTestHooks() TestHooks {
	return r.hookImpl
}

func (r *Replayer) Start(ctx context.Context) (err error) {

	r.logger.Debug("Starting Keploy replay... Please wait.")

	// The exit code, recorded on EVERY way out of this function.
	//
	// There are nine returns and exactly one of them used to record the run:
	// the ordinary end of the loop. A user abort returned nil; a cancelled
	// context returned the context's error; and a run that never got as far
	// as the loop -- no test sets recorded, the report store unreadable, the
	// agent not coming up -- returned an error that goes nowhere, because
	// `keploy test` swallows Start's error and hands cobra a nil (cli/test.go)
	// so that a failure already reported is not printed a second time with a
	// usage dump. utils.ErrCode is therefore the ONLY thing left that can make
	// the process exit non-zero, and none of those paths set it: a suite
	// already red and then interrupted, and a CI job whose tests were never
	// recorded, both exited 0.
	//
	// Registered at the TOP and driven off the named return, so it covers the
	// returns that happen before there is any verdict to speak of, and so the
	// next early return added here cannot forget it.
	//
	// parentCtx is the context as passed into Start — canceled only by a
	// real user interrupt (SIGINT via utils.NewCtx). The errgroup-derived
	// ctx below additionally cancels on ANY goroutine error, so it must NOT
	// be used to detect user-abort: doing so would suppress TestRunAborted
	// for exactly the internal graceful-abort paths this telemetry targets.
	//
	// Captured before this defer, which reads it: `g, ctx :=` below reassigns
	// ctx in this same scope, and the teardown defer cancels that one before
	// this defer runs. Pinned by TestStartThatRunsNoTestExitsNonZero.
	parentCtx := ctx
	testRunResult := true
	defer func() {
		r.completeTestReportMu.RLock()
		failed := r.totalTestFailed
		r.completeTestReportMu.RUnlock()
		// A cancelled ROOT context is not a failure -- cancelled is not
		// failed, and under a signal the error Start returns is usually the
		// cancellation itself. What a Ctrl+C must not do is erase a failure
		// already reached, and testRunResult and failed carry that. A
		// DEADLINE, on the root or below it, is a different thing and still
		// counts: only the root being cancelled is a user saying stop.
		runErr := err
		if stoppedByItsUser(parentCtx) {
			runErr = nil
		}
		armRunExitCode(runErr, testRunResult, failed)
	}()

	// A mock.real keploy cannot read is refused before anything runs (proven
	// switch only). This run's first push starts the agent's REAL
	// bookkeeping afresh.
	if err := realclient.Check(r.mockReal()); err != nil {
		utils.LogError(r.logger, err, "keploy cannot read keploy.yml mock.real")
		return err
	}
	r.realPushed.Store(false)
	if r.noAgent && len(r.mockReal()) > 0 {
		r.logger.Info("this run reaches the app at its base path with no keploy agent, so mock.real is not checked: no REAL rows, and no destination is required to be reached")
	}

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, g))

	setupErrGrp, _ := errgroup.WithContext(ctx)
	setupCtx := context.WithoutCancel(ctx)
	setupCtx, setupCtxCancel := context.WithCancel(setupCtx)
	setupCtx = context.WithValue(setupCtx, models.ErrGroupKey, setupErrGrp)

	var hookCancel context.CancelFunc
	var stopReason = "replay completed successfully"
	// summaryEmitted flips true once the normal TestRun summary fires. If the
	// run stops gracefully before that (setup/instrument failure), the defer
	// emits a TestRunAborted with the categorized reason instead.
	var summaryEmitted bool

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	defer func() {
		select {
		case <-parentCtx.Done():
			break
		default:
			r.logger.Info("stopping Keploy", zap.String("reason", stopReason))
			// Keploy-initiated (not user Ctrl+C, which lands in the
			// parentCtx.Done case above) graceful stop before any run summary
			// was emitted — record why so the replay funnel can see where test
			// runs die on setup. Graceful only; hard crashes are covered by
			// Sentry. parentCtx (not the errgroup ctx) is checked so internal
			// goroutine errors don't masquerade as user interrupts.
			if !summaryEmitted {
				r.telemetry.TestRunAborted(stopReason)
				if s, ok := r.telemetry.(interface{ Shutdown() }); ok {
					s.Shutdown()
				}
			}
		}

		// Notify the agent that we are shutting down gracefully. It covers early exits before RunTestSet runs
		// and shutdown paths where the per‑test‑set defer doesn’t execute (or never starts).
		// Bound it: context.Background() with no deadline meant an up-but-unresponsive
		// agent (common while it is still booting under CPU contention) could block this
		// POST — and therefore the whole teardown, which is the path SIGINT takes — forever.
		notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := r.instrumentation.NotifyGracefulShutdown(notifyCtx); err != nil {
			r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
		}
		notifyCancel()

		if hookCancel != nil {
			hookCancel()
		}
		cancel()
		// Bounded drain: a setup/run goroutine that wedges under contention and does
		// not observe cancel() must not be able to block this teardown forever, or
		// SIGINT (which runs through this same defer) is swallowed and the process
		// hangs until an external SIGKILL. See utils.DrainErrGroup.
		if err := utils.DrainErrGroup(r.logger, "replay", g, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "failed to stop replaying")
		}

		setupCtxCancel()
		if err := utils.DrainErrGroup(r.logger, "replay-setup", setupErrGrp, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "failed to stop replaying")
		}
	}()

	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to get all test set ids: %v", err)
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	if len(testSetIDs) == 0 {
		recordCmd := models.HighlightGrayString("keploy record")
		errMsg := fmt.Sprintf("No test sets found in the keploy folder. Please record testcases using %s command", recordCmd)
		utils.LogError(r.logger, err, errMsg)
		// Ran `keploy test` before any tests were recorded — a distinct
		// funnel signal, so surface it via the categorized stop reason
		// rather than letting the default "completed" mask it.
		stopReason = "no test sets found"
		return fmt.Errorf("%s", errMsg)
	}

	// The test sets this run replays, refused before anything starts when
	// one it is asked for is not recorded.
	selectedSets, err := r.selectTestSets(testSetIDs)
	if err != nil {
		stopReason = "a selected test set is not recorded"
		return err
	}
	r.logger.Info("Test Sets to be Replayed", zap.Strings("testSets", selectedSets))

	// The base path is read once, here, before any test is sent anywhere: one
	// keploy cannot read is put in no test's URL, and leaves every test going
	// to the address it was recorded at, or to test.host.
	if basePath := r.config.Test.BasePath; basePath != "" {
		if _, err := parseBasePath(basePath); err != nil {
			// The reason goes out as telemetry too, so it leaves out the base
			// path itself, which names a host of the user's.
			stopReason = "cannot read the base path"
			utils.LogError(r.logger, err, "cannot replay against the base path",
				zap.String("next_step", "write --base-path / test.basePath as an http or https URL with a host, as in http://localhost:8080 or https://staging.example.com/api, or as a path prefix such as /api"))
			return fmt.Errorf("cannot replay against the base path: %w", err)
		}
	}

	r.completeTestReportMu.Lock()
	r.completeTestReport = make(map[string]TestReportVerdict)
	r.totalTests, r.totalTestPassed, r.totalTestFailed, r.totalTestObsolete, r.totalTestIgnored = 0, 0, 0, 0, 0
	r.consumedMockNames = make(map[string]struct{})
	r.totalTestTimeTaken = 0
	r.completeTestReportMu.Unlock()
	r.forgetQuarantined()
	r.failedTCsBySetID = make(map[string][]string)
	r.mockMismatchFailures = NewTestFailureStore()

	testRunID, err := r.GetNextTestRunID(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to get next test run id: %v", err)
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	var language models.Language
	var executable string
	// only find language to calculate coverage if instrument is true
	if r.instrument {
		language, executable = utils.DetectLanguage(r.logger, r.config.Command)
		// if language is not provided and language detected is known
		// then set the language to detected language
		if r.config.Test.Language == "" {
			if language == models.Unknown {
				r.logger.Debug("failed to detect language, skipping coverage calculation. please use --language to manually set the language")
				r.config.Test.SkipCoverage = true
			} else {
				r.logger.Debug(fmt.Sprintf("%s language detected. please use --language to manually set the language if needed", language))
			}
			r.config.Test.Language = language
		} else if language != r.config.Test.Language && language != models.Unknown {
			utils.LogError(r.logger, nil, "language detected is different from the language provided")
			r.config.Test.SkipCoverage = true
		}
	}

	r.logger.Debug("language detected", zap.String("language", r.config.Test.Language.String()), zap.String("executable", executable))

	var cov coverage.Service
	switch r.config.Test.Language {
	case models.Go:
		cov = golang.New(ctx, r.logger, r.reportDB, r.config.Command, r.config.Test.CoverageReportPath, r.config.CommandType)
	case models.Python:
		// if the executable is not starting with "python" or "python3" then skipCoverage
		if !strings.HasPrefix(executable, "python") && !strings.HasPrefix(executable, "python3") {
			r.logger.Debug("python command not python or python3, skipping coverage calculation")
			r.config.Test.SkipCoverage = true
		}
		cov = python.New(ctx, r.logger, r.reportDB, r.config.Command, executable)
	case models.Javascript:
		cov = javascript.New(ctx, r.logger, r.reportDB, r.config.Command)
	case models.Java:
		cov = java.New(ctx, r.logger, r.reportDB, r.config.Command, r.config.Test.JacocoAgentPath, executable)
	default:
		r.config.Test.SkipCoverage = true
	}
	if !r.config.Test.SkipCoverage {
		if utils.CmdType(r.config.CommandType) == utils.Native {
			r.config.Command, err = cov.PreProcess(r.config.Test.DisableLineCoverage)

			if err != nil {
				r.config.Test.SkipCoverage = true
			}
		}
		err = os.Setenv("CLEAN", "true") // related to javascript coverage calculation
		if err != nil {
			r.config.Test.SkipCoverage = true
			r.logger.Debug("failed to set CLEAN env variable, skipping coverage calculation", zap.Error(err))
		}
	}

	// Instrument will load the hooks and start the proxy
	inst, err := r.Instrument(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to instrument: %v", err)
		utils.LogError(r.logger, err, stopReason)
		if ctx.Err() == context.Canceled {
			return err
		}
		// The error Start returns stays flat (#4792), so the code the setup's
		// failure is tagged with (an agent that lacked privileges: 3) is
		// armed here, while the tag is still in hand; armRunExitCode keeps it.
		utils.SetExitCodeOnce(utils.ExitCodeFor(err))
		return fmt.Errorf("%s", stopReason)
	}

	hookCancel = inst.HookCancel

	// Hoist cmdType up to before the test-set loop so the one-shot
	// user-app spawn below can read it. The original assignment that
	// lived just above the for-range over testSets is removed (kept
	// in a single canonical location here).
	cmdType := utils.CmdType(r.config.CommandType)

	// The agent is up, and the application it serves has not started (not
	// even the --keep-app-alive one below): the run's refused PID map inserts
	// are counted from here. A compose agent comes up with its project, in
	// RunTestSet, which marks it there.
	r.pidMarked, r.lossMarked = false, false
	r.lossRunBase, r.lossRunBaseSet = nil, false
	if r.instrument && cmdType != utils.DockerCompose {
		r.markPIDMaps(ctx)
	}

	// --keep-app-alive: start the user app ONCE on the outer errgroup
	// so its lifecycle is tied to the whole replay run rather than a
	// single test-set. The per-test-set RunApplication spawn (and its
	// sibling NotifyGracefulShutdown notify) below is gated off via
	// the existing `serveTest` parameter on RunTestSet — we pass
	// `effectiveKeepAlive` as that parameter at the call site (line
	// ~480). Keeps long-lived TCP connections (asyncpg pool, HikariCP,
	// etc.) warm across the test-set boundary — the precondition the
	// cross-test-set session/startup-tier staleness bugs in
	// keploy/integrations#203 need to surface.
	//
	// Supported command types: docker-compose, docker-run,
	// docker-start, native. All of them ultimately call
	// r.instrumentation.Run(ctx, opts) — the only difference is what
	// child command runs — so the same one-shot spawn pattern works
	// for every cmdType. cmdType == Empty (no -c) means there is no
	// user application to manage; the same gate ALSO suppresses
	// passing serveTest=true to RunTestSet (which would otherwise skip
	// the per-test-set RunApplication path and leave the app
	// unstarted), and the per-test-set waitForAppReady skip below
	// stays armed only when the one-shot spawn actually fired.
	// Single computed predicate so all three gates move together.
	//
	// Docker-compose replay reuses the stack across test-sets BY DEFAULT (when
	// mocking is on): otherwise keploy tears down + recreates the whole
	// agent+app+network+volumes for every test-set, multiplying docker-daemon
	// create/destroy churn N-fold and re-paying a per-test-set agent-readiness
	// window — under loaded CI that churn is what blows past the readiness
	// timeout. The agent is reused safely: each test-set's mocks are re-scoped
	// in-band per set (MockOutgoing→ResetForReplaySession, StoreMocks,
	// SendMockFilterParamsToAgent), the same contract the runner already relies
	// on. Gated on Mocking so a replay against a real (unmocked) dependency —
	// where surviving app state across the boundary could change expected
	// before-state — still restarts per test-set. An explicit --keep-app-alive
	// keeps working unchanged (OR can only widen).
	composeReuse := cmdType == utils.DockerCompose && r.config.Test.Mocking
	effectiveKeepAlive := (r.config.Test.KeepAppAlive || composeReuse) && r.instrument && cmdType != utils.Empty

	// Records a --keep-app-alive app failure so the run can exit non-zero on
	// it. The errgroup alone cannot do that: nothing ever calls g.Wait(), so
	// the error this goroutine returns is collected by no one, and the exit
	// code is carried by utils.ErrCode rather than by Start's return value.
	var keepAliveAppErr atomic.Pointer[models.AppError]

	// This run's last set, and its final attempt, are this run's alone.
	r.keepAliveStop, r.isLastTestSet, r.finalAttempt = nil, false, false
	if effectiveKeepAlive {
		// Under the proven switch the app runs under a context of its own,
		// so the last set's end can stop it, and wait for it, before its
		// read (P101-c), as a set stops the app it started (P101-b).
		kaCtx, stopKeepAlive, keepAliveRan := keepAliveStopper(ctx, cmdType)
		r.keepAliveStop = stopKeepAlive
		g.Go(func() error {
			var appErr models.AppError
			defer func() { keepAliveRan(appErr) }()
			defer utils.Recover(r.logger)
			appErr = r.RunApplication(kaCtx, models.RunOptions{
				AppCommand: r.config.Command,
			})
			// Two outcomes that are NOT failures from the replay's
			// point of view:
			//   1. Zero-value AppError: RunApplication returned cleanly.
			//   2. ErrCtxCanceled: ctx was cancelled (normal end-of-run
			//      teardown — the outer Start() defers a graceful
			//      shutdown notify; the app exits as a consequence).
			// Either case → return nil; errgroup keeps the run alive.
			if appErr == (models.AppError{}) || appErr.AppErrorType == models.ErrCtxCanceled {
				return nil
			}
			// Anything else is an app startup / crash failure. Propagate
			// it back through the errgroup so g.Wait() at the bottom of
			// Start() surfaces the cause as the run's exit reason and
			// any sibling goroutine listening on the errgroup-derived
			// context unblocks. Logging the failure here too because
			// errgroup only ever returns the FIRST error — if the test
			// loop trips on a parallel error first, the actionable
			// next_step that explains WHY tests started failing en
			// masse would otherwise be hidden.
			utils.LogError(r.logger, fmt.Errorf("user application failed under --keep-app-alive: %v", appErr),
				applicationFailedToRunLogMessage,
				zap.String("kind", string(appErr.AppErrorType)),
				zap.Any("err", appErr),
				zap.String("next_step",
					"the user application started by --keep-app-alive exited or failed to start during the replay run. "+
						"Check the application's own logs (the -c command's stdout/stderr is captured by keploy) for the root cause. "+
						"Common causes: image build failure, port already in use, missing env var, dependency container crash-looped. "+
						"If the app is expected to manage its own lifecycle across test-sets, drop --keep-app-alive and let keploy "+
						"restart it per test-set instead."))
			keepAliveAppErr.Store(&appErr)
			return appErr
		})
	}

	var testSetResult bool
	abortTestRun := false
	r.afterTestRunCalled = false
	var flakyTestSets []string
	var testSets []string
	runDomainSet := telemetry.NewDomainSet()
	r.runDomainSet = runDomainSet
	testSets = selectedSets
	// cmdType is hoisted above the one-shot RunApplication block; the
	// original assignment that lived here is removed to keep a single
	// canonical declaration.
	r.testRunTestSets = testSets
	r.testRunID = testRunID
	r.firstRun = true
	for i, testSet := range testSets {
		var backupCreated bool
		testSetResult = false

		err := r.hookImpl.BeforeTestSetRun(ctx, testSet)
		if err != nil {
			stopReason = fmt.Sprintf("failed to run before test hook: %v", err)
			utils.LogError(r.logger, err, stopReason)
			if ctx.Err() == context.Canceled {
				return err
			}
			return fmt.Errorf("%s", stopReason)
		}

		if !r.config.Test.SkipCoverage {
			err = os.Setenv("TESTSETID", testSet) // related to java coverage calculation
			if err != nil {
				r.config.Test.SkipCoverage = true
				r.logger.Debug("failed to set TESTSETID env variable, skipping coverage calculation", zap.Error(err))
			}
		}

		// check if its the last testset running -

		if i == len(testSets)-1 {
			r.isLastTestSet = true
		}
		// Mirror of isLastTestSet but for the first-testset boundary.
		// Set BEFORE the RunTestSet call so the waitForAppReady call
		// sites in RunTestSet observe the right value. Reset only at
		// the top of this iteration (so a flaky-retry attempt within
		// the SAME iteration still observes the same value).
		r.isFirstTestSet = (i == 0)

		r.completeTestReportMu.RLock()
		initTotal := r.totalTests
		initPassed := r.totalTestPassed
		initFailed := r.totalTestFailed
		initObsolete := r.totalTestObsolete
		initIgnored := r.totalTestIgnored
		initTimeTaken := r.totalTestTimeTaken
		// Snapshot a copy of the run-level distinct mock set so per-attempt
		// additions can be reverted on retry just like the int totals above.
		initConsumedMockNames := make(map[string]struct{}, len(r.consumedMockNames))
		for k := range r.consumedMockNames {
			initConsumedMockNames[k] = struct{}{}
		}
		r.completeTestReportMu.RUnlock()

		var initialFailedTCs map[string]bool
		flaky := false // only be changed during replay with --must-pass flag set
		// The refused test cases --must-pass keeps are said once per set.
		keptRefused := false
		for attempt := 1; attempt <= int(r.config.Test.MaxFlakyChecks); attempt++ {

			// clearing testcase from map is required for 2 reasons:
			// 1st: in next attempt, we need to append results in a fresh array,
			// rather than appending in the old array which would contain outdated tc results.
			// 2nd: in must-pass mode, we delete the failed testcases from the map
			// if the array has some failed testcases, which has already been removed, then not cleaning
			// the array would mean deleting the already deleted failed testcases again (error).
			r.reportDB.ClearTestCaseResults(ctx, testRunID, testSet)

			// overwrite with values before testset run, so after all reruns we don't get a cummulative value
			// gathered from rerunning, instead only metrics from the last rerun would get added to the variables.
			r.completeTestReportMu.Lock()
			r.totalTests = initTotal
			r.totalTestPassed = initPassed
			r.totalTestFailed = initFailed
			r.totalTestObsolete = initObsolete
			r.totalTestIgnored = initIgnored
			r.totalTestTimeTaken = initTimeTaken
			// Re-clone from the snapshot so the next RunTestSet attempt
			// mutates a fresh per-attempt copy without polluting the
			// preserved snapshot for any further retry.
			r.consumedMockNames = make(map[string]struct{}, len(initConsumedMockNames))
			for k := range initConsumedMockNames {
				r.consumedMockNames[k] = struct{}{}
			}
			r.completeTestReportMu.Unlock()

			r.logger.Info("running", zap.String("test-set", models.HighlightString(testSet)), zap.Int("attempt", attempt))
			// serveTest=true reuses the pre-existing gating inside
			// RunTestSet (skips per-test-set RunApplication spawn and
			// NotifyGracefulShutdown). Pass `effectiveKeepAlive` —
			// not the raw config bool — so a `--keep-app-alive` set
			// with `--cmd-type=""` (or with instrument disabled) does
			// NOT skip per-test-set RunApplication while the one-shot
			// spawn was also suppressed; that combination would leave
			// the app entirely unstarted. With this gate the
			// historical per-test-set restart behaviour is preserved
			// whenever the one-shot spawn didn't fire.
			r.setEnd = nil
			r.finalAttempt = noAttemptFollows(attempt, r.config.Test)
			testSetStatus, err := r.RunTestSet(ctx, testSet, testRunID, effectiveKeepAlive)
			r.tellSetEnd(testSet, testSetStatus, err)
			if err != nil {
				if ctx.Err() == context.Canceled {
					return err
				}
				// DockerCompose: when the user-app container exits,
				// compose's --abort-on-container-exit cancels our inner
				// runTestSetCtx. RunTestSet then returns one of three
				// statuses depending on which goroutine wins the race —
				// AppHalted, FaultUserApp, or UserAbort (the last one
				// happens at line ~964 when the ctx-Done select case
				// fires before the appErr-status mapping). The outer
				// ctx is intact (we already early-returned above if it
				// was), so all three are recoverable: the next test set
				// builds a fresh compose. Other errors still abort.
				if cmdType == utils.DockerCompose &&
					(testSetStatus == models.TestSetStatusAppHalted ||
						testSetStatus == models.TestSetStatusFaultUserApp ||
						testSetStatus == models.TestSetStatusUserAbort) {
					utils.LogError(r.logger, err,
						"test set failed because the app exited; continuing to next test set",
						zap.String("test-set", testSet),
						zap.String("status", string(testSetStatus)))
					testSetResult = false
					testRunResult = false
					break
				}
				stopReason = fmt.Sprintf("failed to run test set: %v", err)
				utils.LogError(r.logger, err, stopReason)
				return fmt.Errorf("%s", stopReason)
			}
			switch testSetStatus {
			case models.TestSetStatusAppHalted:
				testSetResult = false
				abortTestRun = shouldAbortTestRun(testSetStatus, cmdType)
			case models.TestSetStatusInternalErr:
				testSetResult = false
				abortTestRun = abortAfterTestSet(testSetStatus, cmdType, r.notJudged(testSet))
			case models.TestSetStatusFaultUserApp:
				testSetResult = false
				abortTestRun = shouldAbortTestRun(testSetStatus, cmdType)
			case models.TestSetStatusUserAbort:
				// A Ctrl+C does not un-fail the tests that already ran; the
				// defer at the top of Start records them.
				//
				// It stays a nil return, so a run interrupted with nothing
				// wrong yet still exits 0 -- cancelled is not failed, and
				// that is the contract the rest of the CLI is built on (the
				// enterprise binary reads its own `interrupted` reason for
				// the same distinction). What is fixed here is only that a
				// FAILURE already reached is no longer erased by the signal.
				return nil
			case models.TestSetStatusFailed:
				testSetResult = false
			case models.TestSetStatusPassed:
				testSetResult = true
			case models.TestSetStatusIgnored:
				testSetResult = false
			case models.TestSetStatusNoTestsToRun:
				testSetResult = false
			}

			if testSetStatus != models.TestSetStatusIgnored {
				testRunResult = testRunResult && testSetResult
				if abortTestRun {
					break
				}
			} else if provenswitch.On() {
				// An IGNORED set has nothing a flaky check could find, and a
				// set IGNORED under the switch ran its app to find none of its
				// tests had a verdict: another attempt only runs it again.
				// (Without the switch an IGNORED set is one the run skips
				// whole, which starts nothing.)
				break
			}

			tcResults, err := r.reportDB.GetTestCaseResults(ctx, testRunID, testSet)
			if err != nil {
				if r.saysResultsUnread(testSetStatus, testSet) {
					utils.LogError(r.logger, err, "failed to get testcase results")
				}
				break
			}
			failedTcIDs := getFailedTCs(tcResults)
			r.failedTCsBySetID[testSet] = failedTcIDs

			// checking for flakiness when --must-pass flag is not set
			// else if --must-pass is set, delete the failed testcases and rerun
			if !r.config.Test.MustPass {
				// populate the map only once at first iteration for flakiness test
				if attempt == 1 {
					initialFailedTCs = make(map[string]bool)
					for _, id := range failedTcIDs {
						initialFailedTCs[id] = true
					}
					continue
				}
				// checking if there is no mismatch in failed testcases across max retries
				// check both length and value
				if len(failedTcIDs) != len(initialFailedTCs) {
					utils.LogError(r.logger, nil, "the testset is flaky, rerun the testset with --must-pass flag to remove flaky testcases", zap.String("testSet", testSet))
					// don't run more attempts if the testset is flaky
					flakyTestSets = append(flakyTestSets, testSet)
					break
				}
				for _, id := range failedTcIDs {
					if _, ok := initialFailedTCs[id]; !ok {
						flaky = true
						utils.LogError(r.logger, nil, "the testset is flaky, rerun the testset with --must-pass flag to remove flaky testcases", zap.String("testSet", testSet))
						break
					}
				}
				if flaky {
					// don't run more attempts if the testset is flaky
					flakyTestSets = append(flakyTestSets, testSet)
					break
				}
				continue
			}

			// this would be executed only when --must-pass flag is set
			// we would be removing failed testcases
			if r.config.Test.MaxFailAttempts == 0 {
				utils.LogError(r.logger, nil, "no. of testset failure occurred during rerun reached maximum limit, testset still failing, increase count of maxFailureAttempts", zap.String("testSet", testSet))
				break
			}
			// A test case refused when its set loaded was never sent, so
			// there is nothing to remove it for: it is kept, and said.
			failedTcIDs, refusedTcIDs := mustPassDeletions(tcResults)
			if len(refusedTcIDs) > 0 && !keptRefused {
				keptRefused = true
				r.logger.Warn("--must-pass keeps the test cases refused when their test set loaded: they were never sent, so they are not removed, and the set fails until they are mended",
					zap.String("testSet", testSet), zap.Strings("testCaseIDs", refusedTcIDs),
					zap.String("next_step", "do what each one's refusal in its report says (edit its placeholder, or put a half-written set back), then run keploy test again"))
			}
			if len(failedTcIDs) == 0 {
				if len(refusedTcIDs) > 0 {
					// Only refusals fail, and no rerun can change one.
					break
				}
				// if no testcase failed in this attempt move to next attempt
				continue
			}

			if !backupCreated {
				if err := r.createBackup(testSet); err != nil {
					utils.LogError(r.logger, err, "failed to create backup, proceeding with test case deletion", zap.String("testSet", testSet))
				}
				backupCreated = true
			}

			r.logger.Info("deleting failing testcases", zap.String("testSet", testSet), zap.Strings("testCaseIDs", failedTcIDs))

			if err := r.testDB.DeleteTests(ctx, testSet, failedTcIDs); err != nil {
				utils.LogError(r.logger, err, "failed to delete failing testcases", zap.String("testSet", testSet), zap.Strings("testCaseIDs", failedTcIDs))
				break
			}
			// after deleting rerun it maxFlakyChecks times to be sure that no further testcase fails
			// and if it does then delete those failing testcases and rerun it again maxFlakyChecks times
			r.config.Test.MaxFailAttempts--
			attempt = 0
		}

		if abortTestRun {
			break
		}

		err = r.hookImpl.AfterTestSetRun(ctx, testSet, testSetResult)
		if err != nil {
			utils.LogError(r.logger, err, "failed to execute after test set run hook", zap.String("testSet", testSet))
		}

		if i == 0 && !r.config.Test.SkipCoverage {
			err = os.Setenv("CLEAN", "false") // related to javascript coverage calculation
			if err != nil {
				r.config.Test.SkipCoverage = true
				r.logger.Debug("failed to set CLEAN env variable, skipping coverage calculation.", zap.Error(err))
			}
			err = os.Setenv("APPEND", "--append") // related to python coverage calculation
			if err != nil {
				r.config.Test.SkipCoverage = true
				r.logger.Debug("failed to set APPEND env variable, skipping coverage calculation.", zap.Error(err))
			}
		}
	}
	// A run none of whose tests got a verdict (every test set, or every test,
	// ignored or quarantined) verified nothing, so it does not pass, as a run
	// with no tests to run does not. With the switch on or off: its exit code
	// was 0. A run that failed already says why.
	r.completeTestReportMu.RLock()
	judged := r.totalTestPassed + r.totalTestFailed + r.totalTestObsolete
	r.completeTestReportMu.RUnlock()
	// verifiedNothing: the run fails for this alone, so no mock difference
	// failed it, and its mismatch table (the misses of tests that got no
	// verdict) is not shown as if one had.
	verifiedNothing := false
	if testRunResult && judged == 0 {
		testRunResult, verifiedNothing = false, true
		r.logger.Error("no test ran or got a verdict: every test set, or every test, of this run was ignored, so it verified nothing; the run fails",
			zap.Strings("test_sets", testSets),
			zap.String("next_step", "take a test set or test out of test.ignoredTests (or out of quarantine), or run only the test sets that should run with --test-sets"))
	}
	if !r.config.Test.SkipCoverage && r.config.Test.Language == models.Java {
		err = java.MergeAndGenerateJacocoReport(ctx, r.logger)
		if err != nil {
			r.config.Test.SkipCoverage = true
		}
	}

	if len(flakyTestSets) > 0 {
		r.logger.Info("flaky testsets detected, please rerun the specific testsets with --must-pass flag to remove flaky testcases", zap.Strings("testSets", flakyTestSets))
	}

	testRunStatus := "fail"
	if testRunResult {
		testRunStatus = "pass"
	}

	r.completeTestReportMu.RLock()
	passed := r.totalTestPassed
	failed := r.totalTestFailed
	mocksConsumed := len(r.consumedMockNames)
	r.completeTestReportMu.RUnlock()
	r.telemetry.TestRun(passed, failed, len(testSets), mocksConsumed, testRunStatus, map[string]interface{}{
		"host-domains": runDomainSet.ToSlice(),
	})
	// The run reached its summary; the teardown defer must not also emit a
	// TestRunAborted for this invocation.
	summaryEmitted = true
	// Shutdown is optional: the static Telemetry interface does not require it,
	// but the concrete implementation exposes it for graceful drain of in-flight events.
	if s, ok := r.telemetry.(interface{ Shutdown() }); ok {
		s.Shutdown()
	}

	if !abortTestRun {
		r.printSummary(ctx, testRunResult)

		// Print the mismatch table whenever there ARE mock mismatches — not
		// only when the run as a whole failed. A green run with mock misses
		// (e.g. tests demoted to OBSOLETE, or a protocol whose misses can't
		// fail a test) is exactly the case the user must not stay blind to.
		//
		// Exception on green runs: DNS misses are answered with a synthetic
		// response by design (the app keeps working), so on a fully passing
		// run they are routine, not actionable — without this filter every
		// healthy run with app-startup DNS chatter would print the table.
		// So are connections that sent nothing (models.MatchPhaseNoRequest):
		// no call was made, and the warning names their port once per
		// endpoint; a health check would otherwise add a row per connection.
		mismatchRows := r.mockMismatchFailures.GetFailures()
		if testRunResult {
			actionable := make([]TestFailure, 0, len(mismatchRows))
			for _, f := range mismatchRows {
				if f.FailureReason == models.ErrMockNotFound && f.MismatchReport != nil && !f.MismatchReport.CountsAgainstTest() {
					continue
				}
				actionable = append(actionable, f)
			}
			mismatchRows = actionable
		}
		if len(mismatchRows) > 0 && !r.config.DisableMapping && !verifiedNothing {
			failuresByTestSet := make(map[string]bool)
			for _, failure := range mismatchRows {
				failuresByTestSet[failure.TestSetID] = true
			}

			var testSetIDs []string
			for testSetID := range failuresByTestSet {
				testSetIDs = append(testSetIDs, testSetID)
			}
			testSets := strings.Join(testSetIDs, ", ")
			if testRunResult {
				r.logger.Warn("Tests passed, but some outgoing calls did not match the recorded mocks.",
					zap.String("test_sets", testSets),
					zap.String("next_step", "Review the mismatch summary below. Add drifting dynamic fields as noise (test.globalNoise), or re-record the test-set with 'keploy record' if the request structure changed."))
			} else {
				r.logger.Info("Some testsets failed due to mock differences.",
					zap.String("test_sets", testSets),
					zap.String("next_step", "Add drifting dynamic fields as noise (test.globalNoise); if the request structure changed, re-record the test-set with 'keploy record'. A failed test keeps its recorded mapping (--update-test-mapping updates only a passing test's); to remap tests whose mapping is stale, run with --disable-mapping --update-test-mapping, which serves each its recorded window and maps the ones that pass."))
			}

			r.mockMismatchFailures.PrintFailuresTable()
		}
		coverageData := models.TestCoverage{}
		var err error
		if !r.config.Test.SkipCoverage {
			r.logger.Info("calculating coverage for the test run and inserting it into the report")
			coverageData, err = cov.GetCoverage()
			if err == nil {
				r.logger.Sugar().Infoln(models.HighlightPassingString("Total Coverage Percentage: ", coverageData.TotalCov))
				err = cov.AppendCoverage(&coverageData, testRunID)
				if err != nil {
					utils.LogError(r.logger, err, "failed to update report with the coverage data")
				}

			} else {
				r.logger.Debug("failed to calculate coverage for the test run", zap.Error(err))
			}
		}

		//executing afterTestRun hook, executed after running all the test-sets
		if !r.afterTestRunCalled {
			// Under docker compose without a kept stack, the agent is part
			// of each set's stack and goes down with it, so the last set
			// listed calls this hook while its agent is up (RunTestSet).
			// One that ran nothing (ignored, empty, or ended early) leaves
			// no agent to answer here: the run's hooks still run, all but
			// the agent's (withoutAgent).
			hookCtx := ctx
			if cmdType == utils.DockerCompose && r.instrument && !effectiveKeepAlive {
				hookCtx = withoutAgent(ctx)
			}
			err = r.hookImpl.AfterTestRun(hookCtx, testRunID, testSets, coverageData)
			if err != nil {
				utils.LogError(r.logger, err, "failed to execute after test run hook")
			}
		}
	}

	// return non-zero error code so that pipeline processes
	// know that there is a failure in tests
	//
	// SetExitCodeOnce, not a bare assignment: a plain `utils.ErrCode =
	// errCode` also writes ZERO, so a green run wiped a code something else
	// had already armed -- a wrapped runner's own status, or one of Keploy's
	// (utils/exitcodes.go). Arming is one-way here; the defer at the top of
	// Start covers every other way out.
	errCode, runErr := replayRunOutcome(testRunResult, keepAliveAppErr.Load())
	if errCode != 0 {
		utils.SetExitCodeOnce(errCode)
	}
	return runErr
}

// replayRunOutcome decides how a replay run reports itself: the process exit
// code, and the error Start returns.
//
// Two things make this worth naming rather than inlining.
//
// The exit code does NOT travel through Start's return value — it is carried
// by utils.ErrCode, so returning an error alone would still exit 0.
//
// And an app that died under --keep-app-alive is a failed run even when no
// test reported a failure, which is exactly the case that used to escape.
// testRunResult starts true and is only ever set false by a test-set that
// actually RAN, so an app failing to start leaves it true: zero tests
// execute, the summary reports nothing failed, and keploy exits 0 while
// logging "replay completed successfully". Every CI pipeline downstream reads
// that as a pass. The errgroup was supposed to carry the failure — the
// goroutine's own comment says g.Wait() surfaces it — but nothing in this
// package ever calls g.Wait(), so the error had nowhere to go.
func replayRunOutcome(testRunResult bool, keepAliveAppErr *models.AppError) (int, error) {
	if keepAliveAppErr != nil {
		return 1, fmt.Errorf("user application failed under --keep-app-alive: %v", *keepAliveAppErr)
	}
	if !testRunResult {
		return 1, nil
	}
	return 0, nil
}

// armRunExitCode records a failing run in the process exit code.
//
// Both ways out of Start use it -- the ordinary end of the loop and a user
// abort -- so the two cannot drift. They did: the abort path returned with no
// code at all, throwing away the verdict of every test set that had already
// run.
//
// THREE signals, because none of them covers the others.
//
// runErr is how the early returns -- before any test set has a verdict -- say
// the run did not happen: no tests recorded, the report store unreadable, the
// agent never healthy. `keploy test` throws that error away on purpose, so
// without this it reached nothing at all.
//
// testRunResult is the verdict of the test sets that reached one -- but it is
// folded in AFTER the per-set switch, so on every early return it is missing
// the set that was running. A one-set suite, the common shape, looked green
// however many of its tests had already failed.
//
// failedTests is the running count RunTestSet keeps as each test is judged,
// including for the set that was interrupted. It says nothing about a set
// that failed for a reason other than a test -- an app that never came up,
// no tests to run -- which is what testRunResult is for.
//
// A code already armed is left alone. It is more specific than this one:
// a wrapped runner's own status, or one of Keploy's (utils/exitcodes.go) --
// the agent client arms an agent's 3 or 6, and Start arms the code a failed
// setup is tagged with before it flattens that error. That is a deliberate
// change from the unconditional `utils.ErrCode = 1` this replaces.
//
// A run that ended with an error that still carries its tag (one Start
// returns as it is, such as a run the errgroup cancelled) ends with the code
// that tag calls for (utils.ExitCodeFor), not a bare 1.
func armRunExitCode(runErr error, testRunResult bool, failedTests int) {
	switch {
	case runErr != nil:
		utils.SetExitCodeOnce(utils.ExitCodeFor(runErr))
	case !testRunResult || failedTests > 0:
		utils.SetExitCodeOnce(utils.ExitKeployError)
	}
}

func (r *Replayer) Instrument(ctx context.Context) (*InstrumentState, error) {
	if !r.instrument {
		r.logger.Info("Keploy will not mock the outgoing calls when base path is provided", zap.Any("base path", r.config.Test.BasePath))
		return &InstrumentState{}, nil
	}
	passPortsUint := config.GetByPassPorts(r.config)
	passPortsUint32 := make([]uint32, len(passPortsUint)) // slice type of uint32
	for i, port := range passPortsUint {
		passPortsUint32[i] = uint32(port)
	}

	setupOpts := models.SetupOptions{Container: r.config.ContainerName, CommandType: r.config.CommandType, DockerDelay: r.config.BuildDelay, Mode: models.MODE_TEST, BuildDelay: r.config.BuildDelay, EnableTesting: true, GlobalPassthrough: r.config.Record.GlobalPassthrough, DisableHandshakeHold: r.config.Record.DisableHandshakeHold, ChannelBindingShim: r.config.Record.ChannelBindingShim, ConfigPath: r.config.ConfigPath, PassThroughPorts: passPortsUint, InMemoryCompose: r.config.InMemoryCompose}
	// Retry only a stalled agent bring-up (pkg.ErrAgentNotReady) with a fresh
	// agent; a healthy agent is set up once and the test set runs against it.
	err := pkg.RetryAgentSetup(ctx, r.logger, func(c context.Context, _ int) error {
		return r.instrumentation.Setup(c, r.config.Command, setupOpts)
	})
	if err != nil {
		stopReason := "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		// Keep the cause: a setup the user's cancel ended (context.Canceled,
		// from the agent's readiness wait, the retry's backoff, or the docker
		// setup hooks, which get the run's ctx) is the run's cancellation,
		// which Start does not log as a failure; and an agent that could not
		// start is named, with the tag its exit code is read from.
		return &InstrumentState{}, fmt.Errorf("%s: %w", stopReason, err)
	}
	return &InstrumentState{}, nil
}

func (r *Replayer) GetNextTestRunID(ctx context.Context) (string, error) {
	testRunIDs, err := r.reportDB.GetAllTestRunIDs(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		return "", fmt.Errorf("failed to get all test run ids: %w", err)
	}
	return pkg.NextID(testRunIDs, models.TestRunTemplateName), nil
}

func (r *Replayer) GetAllTestSetIDs(ctx context.Context) ([]string, error) {
	return r.testDB.GetAllTestSetIDs(ctx)
}

func (r *Replayer) GetTestCases(ctx context.Context, testID string) ([]*models.TestCase, error) {
	return r.testDB.GetTestCases(ctx, testID)
}

// runsWithoutAgent reports a replay against an app that is already running: a
// base path, and no command for keploy to start. Keploy starts no agent for it
// (see Instrument), so no call the app makes is mocked, and there is nowhere
// to load a mock, nothing to ask what one served, and no app of keploy's to
// watch.
//
// It asks for the base path and not only for the missing command. The base
// path is the user's word that the app is theirs, dependencies and all, which
// is what makes a replay with nothing mocked right. This CLI starts no run
// with neither (it asks for a command); a replayer built that way is left to
// fail on its first call to the agent rather than send the tests, unmocked,
// to whatever answers on the recorded port.
func runsWithoutAgent(cfg *config.Config) bool {
	return cfg.Command == "" && cfg.Test.BasePath != ""
}

// stoppedByItsUser reports whether ctx ended because the run's user stopped
// it: a context above it was cancelled, as utils.NewCtx's is on SIGINT and
// SIGTERM. A deadline is not a stop, and neither is the failure of a goroutine
// the run waits on, which an errgroup gives as the cancellation's cause.
func stoppedByItsUser(ctx context.Context) bool {
	return ctx.Err() != nil && context.Cause(ctx) == context.Canceled
}

// applyBasePath points an HTTP test case's request at test.basePath, when one
// is given. It rewrites the URL in place, so a test case takes it once: a
// second pass would join the base path's own path on again.
//
// A test it returns an error for must not be sent. Its URL still names the
// address it was recorded at, and a base-path run sends each test to the
// address in its URL: the request, which may be a write, would go to the
// recording's environment, and the answer would be compared as the app's.
func (r *Replayer) applyBasePath(testCase *models.TestCase) error {
	if r.config.Test.BasePath == "" || testCase.Kind != models.HTTP {
		return nil
	}
	newURL, err := ReplaceBaseURL(r.config.Test.BasePath, testCase.HTTPReq.URL)
	if err != nil {
		return fmt.Errorf("cannot send the test to the base path %q: %w", r.config.Test.BasePath, err)
	}
	testCase.HTTPReq.URL = newURL
	r.logger.Debug("test case request origin", zap.String("testcase", testCase.Name), zap.String("TestCaseURL", testCase.HTTPReq.URL), zap.String("basePath", r.config.Test.BasePath))
	return nil
}

// failUnsentTest reports a test that was not sent to the app as failed, with
// why, and returns the error of writing that down.
func (r *Replayer) failUnsentTest(ctx context.Context, testRunID, testSetID string, testCase *models.TestCase, why error) error {
	utils.LogError(r.logger, why, "failed the test without sending it",
		zap.String("testcase", testCase.Name),
		zap.String("next_step", "write the test case's request URL with its scheme and host, as in http://localhost:8080/orders: they are what the base path replaces"))
	result := r.CreateFailedTestResult(testCase, testSetID, time.Now().UTC(), why)
	if err := r.reportDB.InsertTestCaseResult(ctx, testRunID, testSetID, result); err != nil {
		utils.LogError(r.logger, err, "failed to insert test case result for a test that was not sent")
		return err
	}
	return nil
}

// beforeFirstTestSet runs the BeforeTestRun hook ahead of the run's first test
// set, and of that one only.
func (r *Replayer) beforeFirstTestSet(ctx context.Context, testRunID string) {
	if !r.firstRun {
		return
	}
	if err := r.hookImpl.BeforeTestRun(ctx, testRunID); err != nil {
		stopReason := fmt.Sprintf("failed to run before test run hook: %v", err)
		utils.LogError(r.logger, err, stopReason)
	}
	r.firstRun = false
}

// testSetDrainTimeout bounds the drain of a test set's goroutines (the
// application and its watcher) once the set is over. A var, not a const, so a
// test can shrink it rather than wait it out.
var testSetDrainTimeout = 30 * time.Second

func (r *Replayer) RunTestSet(ctx context.Context, testSetID string, testRunID string, serveTest bool) (models.TestSetStatus, error) {
	// Whether this run of the set is judged is this run's to say: a mark left
	// by an earlier attempt, or an earlier Start, says nothing about it.
	r.forgetNotJudged(testSetID)

	// Under the proven switch the set is judged only if one agent process
	// answered for all of it (watchAgent): the watch begins before anything
	// of the set is staged on the agent, and is read after the set's last
	// read of it.
	agentWatch := r.watchAgent()
	if agentWatch != nil {
		defer agentWatch.Stop()
	}

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	runTestSetErrGrp, runTestSetCtx := errgroup.WithContext(ctx)
	runTestSetCtx = context.WithValue(runTestSetCtx, models.ErrGroupKey, runTestSetErrGrp)
	runTestSetCtx, runTestSetCtxCancel := context.WithCancel(runTestSetCtx)

	startTime := time.Now()
	pruneBefore := startTime.UTC()
	// notReady is why the set's application never got ready, under the
	// proven switch (readiness_proven.go): the set runs no test and is not
	// judged.
	notReady := ""

	// exitLoopChan is never closed. The test loops below are its only readers,
	// and they have finished by the time the deferred drain runs; its writers
	// are the app watchers in runTestSetErrGrp, and a drain that times out
	// leaves the group running. Closing it after the drain ordered nothing for
	// the readers, and raced a watcher's send: the race detector caught the
	// close and the send unordered once the drain had given up, and a watcher
	// that sent after the close would have panicked.
	exitLoopChan := make(chan bool, 2)
	defer func() {
		// Notify the agent before cancelling the app context so proxy logs shutdown errors as debug.
		if r.instrument && !serveTest {
			notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := r.instrumentation.NotifyGracefulShutdown(notifyCtx); err != nil {
				r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
			}
			notifyCancel()
		}
		runTestSetCtxCancel()
		// Bounded drain so a wedged per-test-set goroutine can't hang teardown/SIGINT.
		if err := utils.DrainErrGroup(r.logger, "replay-testset", runTestSetErrGrp, testSetDrainTimeout); err != nil {
			utils.LogError(r.logger, err, "error in testLoopErrGrp")
		}
	}()

	testCases, err := r.testDB.GetTestCases(runTestSetCtx, testSetID)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to get test cases: %w", err)
	}
	// A set a killed keploy templatize left half-written is refused, never
	// read as written; a set whose files cannot be read is an error, as one
	// whose test cases cannot be read is.
	if refused, err := r.refuseInterruptedSet(runTestSetCtx, testRunID, testSetID, testCases, startTime); refused || err != nil {
		return models.TestSetStatusFailed, err
	}

	// Extract host domains from test cases for telemetry (HTTP and gRPC only)
	if r.runDomainSet != nil {
		for _, tc := range testCases {
			r.runDomainSet.AddAll(telemetry.ExtractDomainsFromTestCase(tc))
		}
	}

	if len(testCases) == 0 {
		r.logger.Debug("no valid test cases found to run for test set", zap.String("test-set", testSetID))

		testReport := &models.TestReport{
			Version:   models.GetVersion(),
			TestSet:   testSetID,
			Status:    string(models.TestSetStatusNoTestsToRun),
			Total:     0,
			Ignored:   0,
			TimeTaken: time.Since(startTime).String(),
			CmdUsed:   r.config.Test.CmdUsed,
			Proven:    provenswitch.On(),
		}
		r.noteSetEnd(testReport)
		err = r.reportDB.InsertReport(runTestSetCtx, testRunID, testSetID, testReport)
		if err != nil {
			utils.LogError(r.logger, err, "failed to insert report")
			return models.TestSetStatusFailed, err
		}
		return models.TestSetStatusNoTestsToRun, nil
	}

	if _, ok := r.config.Test.IgnoredTests[testSetID]; ok && len(r.config.Test.IgnoredTests[testSetID]) == 0 {
		timeTaken := time.Since(startTime)
		testReport := &models.TestReport{
			Version:   models.GetVersion(),
			TestSet:   testSetID,
			Status:    string(models.TestSetStatusIgnored),
			Total:     len(testCases),
			Ignored:   len(testCases),
			TimeTaken: timeTaken.String(),
			CmdUsed:   r.config.Test.CmdUsed,
			Proven:    provenswitch.On(),
		}
		// Under the proven switch the report names each skipped test, with its
		// reason, instead of counting them.
		if rows := r.ignoredSetResults(testSetID, testCases); rows != nil {
			for i := range rows {
				if err := r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, &rows[i]); err != nil {
					utils.LogError(r.logger, err, "failed to insert test case result")
					return models.TestSetStatusFailed, err
				}
			}
			testReport.Tests = rows
			testReport.Total = len(rows)
			testReport.Ignored = len(rows)
		}

		r.noteSetEnd(testReport)
		err = r.reportDB.InsertReport(runTestSetCtx, testRunID, testSetID, testReport)
		if err != nil {
			utils.LogError(r.logger, err, "failed to insert report")
			return models.TestSetStatusFailed, err
		}

		verdict := TestReportVerdict{
			total:     testReport.Total,
			failed:    0,
			passed:    0,
			obsolete:  0,
			ignored:   testReport.Ignored,
			status:    true,
			duration:  timeTaken,
			timeTaken: timeTaken.String(),
		}

		r.completeTestReportMu.Lock()
		r.completeTestReport[testSetID] = verdict
		r.totalTests += testReport.Total
		r.totalTestIgnored += testReport.Ignored
		r.totalTestTimeTaken += timeTaken
		r.completeTestReportMu.Unlock()

		return models.TestSetStatusIgnored, nil
	}

	var conf *models.TestSet
	if r.testSetConf != nil {
		conf, err = r.testSetConf.Read(runTestSetCtx, testSetID)
		if err != nil {
			if strings.Contains(err.Error(), "no such file or directory") || strings.Contains(err.Error(), "The system cannot find the file specified") {
				r.logger.Info("test-set config file not found, continuing execution...", zap.String("test-set", testSetID))
			} else {
				return models.TestSetStatusFailed, fmt.Errorf("failed to read test set config: %w", err)
			}
		}
	}
	if conf == nil {
		conf = &models.TestSet{}
	}

	// The set's template state for this run: each key at its config.yaml
	// value (rule 5 of pkg/templates). The run's own values live here and
	// nowhere else; config.yaml is never written back.
	tmplRun := templates.NewRun(r.logger, conf.Template, conf.Secret)
	// Placeholders a test case cannot run with are refused now, before any
	// is sent, and each such test case fails with the reason.
	tmplCheck := tmplRun.Check(testCases, templates.CheckOptions{
		IgnoreOrdering: r.responseIgnoreOrdering(),
		Streaming:      pkg.IsHTTPStreamingTestCase,
	})
	r.refuseUntiedArrayUses(testSetID, &tmplCheck, testCases)
	refusedTests := tmplCheck.Refused
	r.logTemplateCheck(testSetID, tmplCheck, testCases)

	if conf.PreScript != "" {
		r.logger.Info("Running Pre-script", zap.String("script", conf.PreScript), zap.String("test-set", testSetID))
		err := r.executeScript(runTestSetCtx, conf.PreScript)
		if err != nil {
			return models.TestSetStatusFaultScript, fmt.Errorf("failed to execute pre-script: %w", err)
		}
	}

	var appErrChan = make(chan models.AppError, 1)
	var appErr models.AppError
	var success int
	var failure int
	var obsolete int
	var ignored int
	var totalConsumedMocks = map[string]models.MockState{}
	// consumers are the tests that consumed each mock, by name: whose an
	// unconsumed test mock went to, when a sibling's (markConsumedBy).
	consumers := map[string][]string{}
	var passingTotalConsumedMocks = map[string]models.MockState{}
	// perTestRegion names the per-test mocks this set hands the agent, the
	// only ones an agent that says so reads consumed entries for (see
	// consumedForAgent). nil until StoreMocks: then everything is sent.
	var perTestRegion map[string]struct{}

	testSetStatus := models.TestSetStatusPassed
	testSetStatusByErrChan := models.TestSetStatusRunning
	var testSetStatusByErrChanMu sync.RWMutex
	var lastAppErr models.AppError
	setErrStatus := func(status models.TestSetStatus) {
		testSetStatusByErrChanMu.Lock()
		testSetStatusByErrChan = status
		testSetStatusByErrChanMu.Unlock()
	}
	setLastAppErr := func(appErr models.AppError) {
		testSetStatusByErrChanMu.Lock()
		lastAppErr = appErr
		testSetStatusByErrChanMu.Unlock()
	}
	getErrStatus := func() models.TestSetStatus {
		testSetStatusByErrChanMu.RLock()
		defer testSetStatusByErrChanMu.RUnlock()
		return testSetStatusByErrChan
	}
	getLastAppErr := func() models.AppError {
		testSetStatusByErrChanMu.RLock()
		defer testSetStatusByErrChanMu.RUnlock()
		return lastAppErr
	}
	// stopAppBeforeSetEnd, under the proven switch, stops the application
	// keploy started for this set as the set's teardown does (an interrupt,
	// the runner's grace, then a kill) and waits for it to exit (P101-b). It
	// returns why the runner had to kill it (pkg/client/app setend_stop.go),
	// "" when it did not. nil when keploy started none here: a reused stack
	// (serveTest), a base path, docker compose.
	var stopAppBeforeSetEnd func() string
	// appStopKilled is that why, as the app's run returned it; read once the
	// run has returned.
	var appStopKilled string

	cmdType := utils.CmdType(r.config.CommandType)
	// Check if mappings are present and decide filtering strategy
	var expectedTestMockMappings map[string][]models.MockEntry
	var useMappingBased bool
	// startupMockNames: the test-set's boot traffic. Merged into every test's
	// expected-name list because the mapping-based loader takes names only.
	var startupMockNames []string
	var isMappingEnabled bool
	isMappingEnabled = !r.config.DisableMapping
	if r.exactServing() {
		// Under the proven switch a test case is served its mapping, always.
		if !isMappingEnabled {
			r.noteMappingAlwaysUsed()
		}
		isMappingEnabled = true
	}
	r.exact = nil
	selectedTests := matcherUtils.ArrayToMap(r.config.Test.SelectedTests[testSetID])
	// Map mock name to Kind for DNS filtering (mappings may have empty Kind)
	mockKindByName := make(map[string]models.Kind)
	// reusableMockNames marks mocks whose recorder-derived tier is reusable
	// across tests (session / connection / config) or app-startup traffic —
	// i.e. NOT per-test. They belong in the mapping (so the pool is complete)
	// but are excluded from the per-test consumed-vs-expected assertion: only
	// per-test mocks are deterministically attributed to a single test, so
	// comparing reusable/startup mocks would falsely demote tests to OBSOLETE
	// (the same reason DNS mocks are excluded). MockEntry carries no tier, so
	// we derive it from the loaded mocks (which do, via TestModeInfo.Lifetime).
	//
	// Under the proven switch it marks the protocol-phase mocks instead
	// (excludedFromTestMockAssertion): a test owns every data-plane mock its
	// mapping lists, whatever tier DeriveLifetime gives it.
	reusableMockNames := make(map[string]bool)
	// mockLookup maps a mock's name to its summary, protocol and target, for
	// the report: the per-test and session mocks as recorded. Built as the
	// mocks are loaded (loadTestSetMocks), or below, before the tests run,
	// when they are not loaded here.
	var mockLookup map[string]mockDisplayInfo
	addKinds := func(mocks []*models.Mock) {
		for _, m := range mocks {
			mockKindByName[m.Name] = m.Kind
			if excludedFromTestMockAssertion(m) {
				reusableMockNames[m.Name] = true
			}
		}
	}
	// A document the decoders skipped is in no pool, but a mapping entry
	// recorded without a kind can still name it. One the assertion leaves out
	// (a connection failure this keploy cannot replay) must stay out of the
	// expected side, so its kind is known by name too. Other skipped kinds
	// keep the reports they had.
	addSkippedKinds := func(skipped map[string]models.Kind) {
		for name, kind := range skipped {
			if _, ok := mockKindByName[name]; !ok && models.ExcludedFromDependencyAssertion(kind) {
				mockKindByName[name] = kind
			}
		}
	}
	// loadMocksForAgent reads the set's mocks for the run (the mocking
	// strategy and the mapping's maps, then the pools), learns their kinds,
	// and stores on the agent what the agent holds: the pools less what this
	// keploy never sends it (models.AgentBound: the connection failures). It
	// returns those, so every count of what the agent holds (the compose
	// guard's among them) agrees with what was sent. The compose and the
	// native path both load through it.
	loadMocksForAgent := func() (filtered, unfiltered []*models.Mock, err error) {
		useMappingBased, expectedTestMockMappings, startupMockNames = r.determineMockingStrategy(ctx, testSetID, isMappingEnabled)
		// fromWindows: under the proven switch, a set with no mappings.yaml,
		// whose test windows say what each test case owns once its mocks are
		// read (windowOwnership).
		var fromWindows bool
		// windowsWhy: why the test windows say what each test case owns.
		var windowsWhy string
		// workerScope: under the proven switch, the set's worker scope, the
		// mocks no test case owns that answer from their place in the set's
		// run on (models.Mapping.Worker).
		var workerScope []models.MockEntry
		if r.exactServing() {
			// Under the proven switch the mapping decides what each test
			// case owns, and a mapping that cannot be read refuses the set.
			if expectedTestMockMappings, workerScope, windowsWhy, err = r.provenMappings(ctx, testSetID); err != nil {
				return nil, nil, err
			}
			useMappingBased = true
			fromWindows = len(expectedTestMockMappings) == 0
		}
		mocksThatHaveMappings := make(map[string]bool)
		mocksWeNeed := make(map[string]bool)

		// Startup mocks are needed by EVERY test, so they go into both maps.
		// mocksThatHaveMappings alone would be wrong: GetFilteredMocks prunes on
		// `isMappedToSpecificTest && !isNeededForCurrentRun`, so a name present
		// only in the first map is dropped whenever a subset of tests is run.
		for _, n := range startupMockNames {
			mocksThatHaveMappings[n] = true
			mocksWeNeed[n] = true
		}

		if isMappingEnabled && len(expectedTestMockMappings) > 0 {
			// Populate the Registry
			for _, mocks := range expectedTestMockMappings {
				for _, m := range mocks {
					mocksThatHaveMappings[m.Name] = true
				}
			}

			if len(selectedTests) > 0 {
				for testID := range selectedTests {
					if mocks, ok := expectedTestMockMappings[testID]; ok {
						for _, m := range mocks {
							mocksWeNeed[m.Name] = true
						}
					}
				}
			} else {
				// Running all tests: every mapped mock is needed. Copy rather
				// than alias — mocksWeNeed was already seeded with the startup
				// names above, and `mocksWeNeed = mocksThatHaveMappings` would
				// alias both variables to one map (harmless today, but it makes
				// the two maps impossible to diverge later).
				for n := range mocksThatHaveMappings {
					mocksWeNeed[n] = true
				}
			}
		}
		// Get all mocks for mapping-based filtering
		filtered, unfiltered, lookup, skipped, err := r.loadTestSetMocks(ctx, testSetID, mocksThatHaveMappings, mocksWeNeed)
		if err != nil {
			return nil, nil, err
		}
		if fromWindows {
			expectedTestMockMappings, workerScope = r.windowOwnership(testSetID, testCases, windowsWhy, skipped.windowed, filtered, unfiltered)
		}
		mockLookup = lookup
		addKinds(filtered)
		addKinds(unfiltered)
		addSkippedKinds(skipped.kinds)
		if filtered == nil && unfiltered == nil {
			r.logger.Debug("no mocks found for test set", zap.String("testSetID", testSetID))
		}
		filtered, unfiltered = models.AgentBound(filtered), models.AgentBound(unfiltered)

		// Extract host domains from mocks for telemetry (HTTP and gRPC only)
		if r.runDomainSet != nil {
			for _, m := range filtered {
				r.runDomainSet.AddAll(telemetry.ExtractDomainsFromMock(m))
			}
			for _, m := range unfiltered {
				r.runDomainSet.AddAll(telemetry.ExtractDomainsFromMock(m))
			}
		}

		if mutator, ok := r.hookImpl.(MockMutator); ok {
			if err := mutator.AfterGetMocks(ctx, filtered, unfiltered); err != nil {
				return nil, nil, err
			}
			// Honour any reusable retagging the mutator applied (e.g. the mongo/v2
			// stable-metadata promotion): move retagged mocks from the per-test pool
			// into the reusable pool so SetMocksWithWindow does not window-filter them.
			filtered, unfiltered = rebalanceReusableMocks(filtered, unfiltered)
		}

		if err := r.instrumentation.StoreMocks(ctx, filtered, unfiltered); err != nil {
			utils.LogError(r.logger, err, "failed to store mocks on agent")
			return nil, nil, err
		}
		perTestRegion = pkg.PerTestRegion(filtered)
		if r.exactServing() {
			r.exact = newExactPlan(expectedTestMockMappings, workerScope, filtered, unfiltered)
			r.exact.noteAhead(expectedTestMockMappings, testCases)
			r.noteOwnsNothing(testSetID)
		}
		return filtered, unfiltered, nil
	}

	if r.instrument && cmdType == utils.DockerCompose {
		if !serveTest {
			runTestSetErrGrp.Go(func() error {
				defer utils.Recover(r.logger)
				appErr = r.RunApplication(runTestSetCtx, models.RunOptions{
					AppCommand: conf.AppCommand,
				})
				if appErr != (models.AppError{}) {
					setLastAppErr(appErr)
				}
				if mappedStatus, ok := mapAppErrorToTestSetStatus(appErr); ok {
					setErrStatus(mappedStatus)
				}
				if appErr.AppErrorType == models.ErrCtxCanceled || appErr == (models.AppError{}) {
					return nil
				}
				appErrChan <- appErr
				return nil
			})
		}

		// Checking for errors in the mocking and application
		runTestSetErrGrp.Go(func() error {
			defer utils.Recover(r.logger)
			select {
			case err := <-appErrChan:
				setLastAppErr(err)
				if mappedStatus, ok := mapAppErrorToTestSetStatus(err); ok {
					setErrStatus(mappedStatus)
				} else if err.AppErrorType == models.ErrCtxCanceled {
					return nil
				}
				utils.LogError(r.logger, err, applicationFailedToRunLogMessage)
			case <-runTestSetCtx.Done():
				setErrStatus(models.TestSetStatusUserAbort)
			}
			exitLoopChan <- true
			runTestSetCtxCancel()
			return nil
		})

		// When the stack is reused across test-sets (serveTest), the agent is
		// started once and stays healthy — only the first test-set needs to wait
		// for readiness. Re-paying a 120s window per test-set is dead time where a
		// transient daemon stall can land. Mirrors the waitForAppReady gating.
		if !serveTest || r.isFirstTestSet {
			// Aligned with the agent's own healthcheck budget; a fixed 120s wait
			// gave up while the agent container was still starting under CI daemon
			// contention. See pkg.AgentReadyTimeout (KEPLOY_AGENT_READY_TIMEOUT).
			agentCtx, cancel := context.WithTimeout(runTestSetCtx, pkg.AgentReadyTimeout())
			defer cancel()

			agentReadyCh := make(chan bool, 1)
			go pkg.AgentHealthTicker(agentCtx, r.logger, string(r.config.Agent.AgentURI), agentReadyCh, 1*time.Second)

			select {
			case <-runTestSetCtx.Done():
				// Parent context cancelled (user pressed Ctrl+C)
				return models.TestSetStatusUserAbort, runTestSetCtx.Err()
			case <-agentCtx.Done():
				// Tear down the compose stack we already created before returning,
				// so a retry's `compose up` doesn't hit "container name already in
				// use" — the readiness timeout otherwise leaves the dependency
				// containers + project network behind. Fresh ctx since
				// runTestSetCtx is being torn down; composeDown bounds itself.
				if downErr := r.instrumentation.ComposeDownOnSetupFailure(context.Background()); downErr != nil {
					r.logger.Debug("composeDown after agent-ready timeout failed", zap.Error(downErr))
				}
				return models.TestSetStatusFailed, fmt.Errorf("keploy-agent did not become ready in time")
			case <-agentReadyCh:
			}
		}

		// In case of Docker Compose : since for every test set the agent and application are restarted, hence each test set can be considered as an indicidual test run
		// We also need the firstRun for knowing the first test set run in the whole test mode for purpose like cleanup
		err := r.hookImpl.BeforeTestSetCompose(ctx, testRunID, testSetID, r.firstRun)
		if err != nil {
			stopReason := fmt.Sprintf("failed to run BeforeTestSetCompose hook: %v", err)
			utils.LogError(r.logger, err, stopReason)
		}
		r.firstRun = false
		// Prepare header + body noise configuration for mock matching
		mockNoiseConfig := PrepareMockNoiseConfig(r.config.Test.GlobalNoise.Global, r.config.Test.GlobalNoise.Testsets, testSetID)

		if r.config.Test.FallBackOnMiss {
			r.fallbackDeprecateOnce.Do(func() {
				r.logger.Info("fallBackOnMiss flag is deprecated and ignored. Replay is now always deterministic. Remove this flag from your config.")
			})
		}

		// Kept in a variable rather than inlined so the post-bring-up
		// re-registration repeats the exact same outgoing options. See
		// ensureAgentHoldsStoredMocks.
		outgoingOpts := models.OutgoingOptions{
			Rules:                     r.config.BypassRules,
			MongoPassword:             r.config.Test.MongoPassword,
			SQLDelay:                  time.Duration(r.config.Test.Delay) * time.Second,
			Mocking:                   r.config.Test.Mocking,
			Backdate:                  testCases[0].HTTPReq.Timestamp,
			NoiseConfig:               mockNoiseConfig,
			DisableAutoHeaderNoise:    r.config.Test.DisableAutoHeaderNoise,
			MockNoiseDetection:        r.config.Test.NoiseDetection(),
			MockNoiseStrict:           r.config.Test.NoiseStrict(),
			Learn:                     r.learning(),
			LearnPool:                 r.learningPool(),
			DisableStatefulMocks:      r.statefulMocksDisabled(),
			DisableMockCorrelation:    r.config.Test.DisableMockCorrelation,
			MysqlPorts:                r.config.MysqlPorts,
			DisableMysqlAutoDetect:    r.config.DisableMysqlAutoDetect,
			DisableMysqlEndpointDrift: r.config.DisableMysqlEndpointDrift,
			DisablePgTimeSliceCohort:  r.config.DisablePgTimeSliceCohort,
			PassThroughPorts:          r.config.Record.PassThroughPorts,
			PassThroughHosts:          r.config.Record.PassThroughHosts,
		}
		err = r.mockOutgoingForTestSet(runTestSetCtx, outgoingOpts)
		if err != nil {
			if ctx.Err() != context.Canceled {
				utils.LogError(r.logger, err, "failed to mock outgoing")
			}
			return models.TestSetStatusFailed, err
		}

		filteredMocks, unfilteredMocks, err := loadMocksForAgent()
		if err != nil {
			return models.TestSetStatusFailed, err
		}

		if !isMappingEnabled {
			r.logger.Debug("Mapping-based mock filtering strategy is disabled, using timestamp-based mock filtering strategy")
		}

		// Send initial filtering parameters to set up mocks for test set
		err = r.SendMockFilterParamsToAgent(ctx, []string{}, models.BaseTime, time.Now(), r.consumedForAgent(totalConsumedMocks, perTestRegion), useMappingBased, r.recordedSetShape(ctx, testSetID, testCases))
		if err != nil {
			return models.TestSetStatusFailed, err
		}

		// The compose agent is up and the app is still parked behind its
		// healthcheck: this test set's refused PID map inserts are counted from
		// here. Only for an agent this test set started; one reused across
		// test sets carries the mark the last test set left, once a test set
		// has taken one (pidMarked: the first test set can end before here).
		if !serveTest || !r.pidMarked {
			r.markPIDMaps(ctx)
		}

		err = r.instrumentation.MakeAgentReadyForDockerCompose(ctx)
		if errors.Is(err, provenswitch.ErrAgentSwitch) {
			// The agent and this keploy are on different proven paths: no
			// verdict of this set could be trusted.
			return models.TestSetStatusInternalErr, err
		}
		if err != nil {
			utils.LogError(r.logger, err, "Failed to make the request to make agent ready for the docker compose")
		}

		// Wait for the user application to become ready before firing the first test.
		// Prefers polling Test.HealthURL when set, otherwise falls back to the fixed --delay sleep.
		//
		// --keep-app-alive: the app was spawned ONCE in Start() and
		// has already passed its readiness check during the first
		// test-set; subsequent test-sets inherit the warm app, so
		// paying --delay seconds (or another HealthURL poll round)
		// per boundary is dead time. Skip when we're past the first
		// test-set. The first test-set still runs the wait so the
		// initial app startup can finish (build/boot/connect) before
		// the first request fires.
		//
		// Gate keyed on `serveTest` (the parameter Start() computes
		// as `effectiveKeepAlive`) rather than the raw config bool,
		// so the skip ONLY arms when the one-shot spawn actually
		// fired. If --keep-app-alive was set on a cmdType where the
		// one-shot spawn was suppressed (cmdType == Empty / instrument
		// off), serveTest will be false here and the readiness wait
		// runs every test-set, matching the historical lifecycle.
		// Under the proven switch every set waits for its application's
		// readiness: a warm one is ready at the first read, and one that
		// never got ready for the first set is not fired at for the next.
		if !waitsForApp(serveTest, r.isFirstTestSet) {
			r.logger.Debug("--keep-app-alive: skipping waitForAppReady on post-first test-set; app already warm")
		} else if ready, why := r.appReady(runTestSetCtx, testCases, testSetID, true, serveTest && !r.isFirstTestSet); !ready && why == "" {
			return models.TestSetStatusUserAbort, context.Canceled
		} else if !ready {
			notReady = why
		}

		// The compose bring-up retry (pkg/client/app) can leave keploy's agent
		// replaced: compose starts again an agent it stopped, or recreates one
		// under a recreate flag, and the replacement boots empty. The CLI sets
		// it up again as this one was (pkg/platform/http/session.go); should that
		// not have taken, everything this session stored on the agent lived in
		// the process that went away, and the tests would fire against an agent
		// holding nothing and report zero executed tests (keploy#4614).
		// Re-register with the replacement, or fail this test set loudly rather
		// than proceeding mockless.
		if err := r.ensureAgentHoldsStoredMocks(ctx, testRunID, testSetID, outgoingOpts, filteredMocks, unfilteredMocks,
			totalConsumedMocks, useMappingBased, testCases, agentWatch); err != nil {
			return models.TestSetStatusFailed, err
		}
	}

	// A base path replays against an app that is already running, with nothing
	// mocked: there are no mocks to load, no agent to hand them to and no app
	// to start. Of the setup below only the run's hook is left.
	if r.noAgent {
		r.beforeFirstTestSet(ctx, testRunID)
	}

	if cmdType != utils.DockerCompose && !r.noAgent {

		filteredMocks, unfilteredMocks, err := loadMocksForAgent()
		if err != nil {
			return models.TestSetStatusFailed, err
		}
		r.beforeFirstTestSet(ctx, testRunID)
		isMappingEnabled := !r.config.DisableMapping

		if !isMappingEnabled {
			r.logger.Debug("Mapping-based mock filtering strategy is disabled, using timestamp-based mock filtering strategy")
		}

		pkg.InitSortCounter(int64(max(len(filteredMocks), len(unfilteredMocks))))

		// Prepare header + body noise configuration for mock matching
		mockNoiseConfig := PrepareMockNoiseConfig(r.config.Test.GlobalNoise.Global, r.config.Test.GlobalNoise.Testsets, testSetID)

		if r.config.Test.FallBackOnMiss {
			r.fallbackDeprecateOnce.Do(func() {
				r.logger.Info("fallBackOnMiss flag is deprecated and ignored. Replay is now always deterministic. Remove this flag from your config.")
			})
		}

		err = r.mockOutgoingForTestSet(runTestSetCtx, models.OutgoingOptions{
			Rules:                     r.config.BypassRules,
			MongoPassword:             r.config.Test.MongoPassword,
			SQLDelay:                  time.Duration(r.config.Test.Delay) * time.Second,
			Mocking:                   r.config.Test.Mocking,
			Backdate:                  testCases[0].HTTPReq.Timestamp,
			NoiseConfig:               mockNoiseConfig,
			DisableAutoHeaderNoise:    r.config.Test.DisableAutoHeaderNoise,
			MockNoiseDetection:        r.config.Test.NoiseDetection(),
			MockNoiseStrict:           r.config.Test.NoiseStrict(),
			Learn:                     r.learning(),
			LearnPool:                 r.learningPool(),
			DisableStatefulMocks:      r.statefulMocksDisabled(),
			DisableMockCorrelation:    r.config.Test.DisableMockCorrelation,
			MysqlPorts:                r.config.MysqlPorts,
			DisableMysqlAutoDetect:    r.config.DisableMysqlAutoDetect,
			DisableMysqlEndpointDrift: r.config.DisableMysqlEndpointDrift,
			DisablePgTimeSliceCohort:  r.config.DisablePgTimeSliceCohort,
			PassThroughPorts:          r.config.Record.PassThroughPorts,
			PassThroughHosts:          r.config.Record.PassThroughHosts,
		})
		if err != nil {
			if ctx.Err() != context.Canceled {
				utils.LogError(r.logger, err, "failed to mock outgoing")
			}
			return models.TestSetStatusFailed, err
		}

		// Send initial filtering parameters to set up mocks for test set
		err = r.SendMockFilterParamsToAgent(ctx, []string{}, models.BaseTime, time.Now(), r.consumedForAgent(totalConsumedMocks, perTestRegion), useMappingBased, r.recordedSetShape(ctx, testSetID, testCases))
		if err != nil {
			return models.TestSetStatusFailed, err
		}

		if r.instrument {
			if !serveTest {
				// Under the proven switch the app runs under a context of its
				// own, so the set's end can stop it, and wait for it, before
				// it reads what the app did after its last test (P101-b). A
				// stop of it is the app's run ending (ErrCtxCanceled), never a
				// halt or an abort of the set.
				//
				// The stop's cause (models.ErrAppStoppedForSetEnd) has the app's
				// runner say, in the run's StopKilled, whether its stop had to
				// kill the app.
				appCtx := runTestSetCtx
				var appExited chan struct{}
				if provenswitch.On() {
					var stopApp context.CancelCauseFunc
					appCtx, stopApp = context.WithCancelCause(runTestSetCtx)
					appExited = make(chan struct{})
					stopAppBeforeSetEnd = func() string {
						stopApp(models.ErrAppStoppedForSetEnd)
						select {
						case <-appExited:
							return appStopKilled
						case <-runTestSetCtx.Done():
							return ""
						}
					}
				}
				// Under the proven switch: the listens the agent saw before
				// this set's application starts are not this one's.
				r.markReadiness(runTestSetCtx)
				runTestSetErrGrp.Go(func() error {
					if appExited != nil {
						defer close(appExited)
					}
					defer utils.Recover(r.logger)
					appErr = r.RunApplication(appCtx, models.RunOptions{
						AppCommand: conf.AppCommand,
					})
					appStopKilled = appErr.StopKilled
					if appErr != (models.AppError{}) {
						setLastAppErr(appErr)
					}
					if mappedStatus, ok := mapAppErrorToTestSetStatus(appErr); ok {
						setErrStatus(mappedStatus)
					}
					if appErr.AppErrorType == models.ErrCtxCanceled || appErr == (models.AppError{}) {
						return nil
					}
					appErrChan <- appErr
					return nil
				})
			}

			// Checking for errors in the mocking and application
			runTestSetErrGrp.Go(func() error {
				defer utils.Recover(r.logger)
				select {
				case err := <-appErrChan:
					setLastAppErr(err)
					if mappedStatus, ok := mapAppErrorToTestSetStatus(err); ok {
						setErrStatus(mappedStatus)
					} else if err.AppErrorType == models.ErrCtxCanceled {
						return nil
					}
					utils.LogError(r.logger, err, applicationFailedToRunLogMessage)
				case <-runTestSetCtx.Done():
					setErrStatus(models.TestSetStatusUserAbort)
				}
				exitLoopChan <- true
				runTestSetCtxCancel()
				return nil
			})

			// Wait for the user application to become ready before firing the first test.
			// Prefers polling Test.HealthURL when set, otherwise falls back to the fixed --delay sleep.
			// See the parallel call in the docker-compose branch above
			// for the --keep-app-alive rationale. On the non-compose
			// paths (native, docker-run, docker-start) the one-shot
			// spawn in Start() also fires when KeepAppAlive is set
			// AND cmdType is non-empty AND instrument is enabled.
			// Gate keyed on `serveTest` (the Start()-computed
			// effectiveKeepAlive predicate) — identical to the compose
			// branch above — so the readiness skip arms only when the
			// one-shot spawn actually fired.
			if !waitsForApp(serveTest, r.isFirstTestSet) {
				r.logger.Debug("--keep-app-alive: skipping waitForAppReady on post-first test-set; app already warm")
			} else if ready, why := r.appReady(runTestSetCtx, testCases, testSetID, true, serveTest && !r.isFirstTestSet); !ready && why == "" {
				return models.TestSetStatusUserAbort, context.Canceled
			} else if !ready {
				notReady = why
			}

		} else if ready, why := r.appReady(runTestSetCtx, testCases, testSetID, false, false); !ready && why == "" {
			// An application keploy did not start (no command): under the
			// proven switch the first test's address must take a connect.
			return models.TestSetStatusUserAbort, context.Canceled
		} else if !ready {
			notReady = why
		}
	}
	// A base path replays against an application already running: under the
	// proven switch, the first test's address must take a connect first.
	if r.noAgent {
		if ready, why := r.appReady(runTestSetCtx, testCases, testSetID, false, false); !ready && why == "" {
			return models.TestSetStatusUserAbort, context.Canceled
		} else if !ready {
			notReady = why
		}
	}

	if err := r.hookImpl.BeforeTestSetReplay(runTestSetCtx, testSetID); err != nil {
		utils.LogError(r.logger, err, "BeforeTestSetReplay hook failed; inspect your custom hook implementation or disable it for this test set if this failure is expected",
			zap.String("testSetID", testSetID),
		)
	}

	ignoredTests := matcherUtils.ArrayToMap(r.config.Test.IgnoredTests[testSetID])

	testCasesCount := len(testCases)

	if len(selectedTests) != 0 {
		testCasesCount = len(selectedTests)
	}

	// Inserting the initial report for the test set
	testReport := &models.TestReport{
		Version:   models.GetVersion(),
		Total:     testCasesCount,
		Status:    string(models.TestStatusRunning),
		TimeTaken: time.Since(startTime).String(),
		CmdUsed:   r.config.Test.CmdUsed,
		Proven:    provenswitch.On(),
	}

	err = r.reportDB.InsertReport(runTestSetCtx, testRunID, testSetID, testReport)
	if err != nil {
		utils.LogError(r.logger, err, "failed to insert report")
		return models.TestSetStatusFailed, err
	}

	// var to exit the loop
	var exitLoop bool
	// An application that never got ready runs no test (proven switch): the
	// set is not judged, below.
	if notReady != "" {
		exitLoop = true
		testSetStatus = models.TestSetStatusInternalErr
	}
	// endedPartWay reports whether the context of a run with no agent has
	// ended before its tests did, and the status the set ends with if so:
	// USER_ABORT when its user stopped it, INTERNAL_ERR for anything else (a
	// deadline, or a goroutine it waits on failing). The test loops stop on
	// it, and a test whose request it cut short gets no verdict: the end of
	// the run is what failed it, or cannot be told from what did. A run with an
	// agent learns the same from the goroutine that watches its app, through
	// exitLoopChan; one with no agent starts no app and has no such goroutine.
	endedPartWay := func() (models.TestSetStatus, bool) {
		switch {
		case !r.noAgent || runTestSetCtx.Err() == nil:
			return "", false
		case stoppedByItsUser(runTestSetCtx):
			return models.TestSetStatusUserAbort, true
		default:
			return models.TestSetStatusInternalErr, true
		}
	}
	// captureOpened is whether a test of this set has opened its capture
	// window: every later test's window carries in the misses made since the
	// previous one closed (openTestErrorCapture).
	var captureOpened bool
	// What the previous set's application missed before its first test is
	// not this set's.
	r.setCalls.reset()
	// var to store the error in the loop
	var loopErr error
	// Add secret files to .gitignore if they exist
	if len(conf.Secret) > 0 {
		err = utils.AddToGitIgnore(r.logger, r.config.Path, "/*/secret.yaml")
		if err != nil {
			r.logger.Debug("Failed to add secret files to .gitignore", zap.Error(err))
		}
	}

	actualTestMockMappings := &models.Mapping{
		Version:   string(models.GetVersion()),
		Kind:      models.MappingKind,
		TestSetID: testSetID,
	}
	var consumedMocks []models.MockState
	// A base path mocks nothing: none was consumed, and no agent could say.
	if !r.noAgent {
		consumedMocks, err = r.hookImpl.GetConsumedMocks(runTestSetCtx) // Getting mocks consumed during initial setup
		if err != nil {
			if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), err); ok {
				testSetStatus = resolvedStatus
				exitLoop = true
			} else {
				utils.LogError(r.logger, err, "failed to get consumed filtered mocks")
			}
		}
	}
	r.logger.Debug("consumed mocks during initial setup",
		zap.String("testSetID", testSetID),
		zap.Int("count", len(consumedMocks)),
		zap.Any("mocks", consumedMocks))
	// Partition the pre-first-test consumption. Everything drained here was
	// consumed BEFORE any test fired, but it is not all the same tier:
	//
	//   - Reusable/session traffic (driver handshakes, auth, connection-pool
	//     warm-up) genuinely belongs to no test — it is what the STARTUP section
	//     below exists to record, and it is folded into totalConsumedMocks as
	//     before.
	//   - PER-TEST single-use mocks can also land here when the app-readiness
	//     gate (waitForAppReady, above) polls Test.HealthURL and that endpoint
	//     calls a mocked dependency. A /health handler that runs e.g. SELECT 1
	//     against a mocked DB makes the proxy serve — and DeleteFilteredMock
	//     permanently consume — a per-test mock that belongs to a recorded test
	//     case (classically the recorded get-health test's own SELECT 1).
	//     Folding THAT into totalConsumedMocks marks it Deleted, so every later
	//     per-test filterOutDeleted strips it and the test that owns it fails
	//     with no_mocks (an intermittent, misattributed "degraded"/500 on the
	//     probe endpoint).
	//
	// So re-arm per-test mocks the gate consumed by WITHHOLDING them from
	// totalConsumedMocks: the next per-test SendMockFilterParamsToAgent rebuilds
	// the serving pool from the stored corpus and, because they are not marked
	// Deleted, re-inserts them so their owning test consumes (and then counts)
	// them. This is the same withhold-to-re-arm effect the per-retry-cycle rewind
	// (rewindConsumedForRetryCycle) uses to re-serve per-test mocks each cycle.
	// The tier is decided by isReusableTierState on the recorder-derived
	// Lifetime/type carried through GetConsumedMocks; a session->per-test mis-tag
	// only re-arms a mock (keeps it available), and the reverse mis-tag is no
	// worse than before this fix (the original fold behaviour).
	//
	// NOTE: this withhold is CLI-side. Under the experimental
	// KEPLOY_AGENT_OWNS_CONSUMED=1 path the agent filters from its own
	// never-rewound consumedPersistent, which the CLI does not touch, so the
	// gate-consumption bug is not yet fixed there (that path is OFF by default;
	// closing it needs an agent-side "un-flag consumed" primitive).
	// A carry-over mock consumed before the first test (the agent flags it
	// CarryOver: a registered kind's, or a StagingRelease need's, such as a
	// message the app received as it started, or a publish it made then) is
	// consumed once. It is not re-armed below as a readiness gate's consume
	// is, which would serve it again in every test: it is kept, and recorded
	// in the startup section, whatever window its recorded time is released
	// in. The app consumed it before any test; a mapping-mode replay names
	// the startup section in every test's call (MergeStartupMockNames), and
	// a fetch of the mapped mocks brings the agent only what a section
	// lists. (The set's staging reaches it by its recorded time.)
	carried, initialConsumed := splitCarriedConsumed(consumedMocks)
	startupConsumed, rearmedConsumed := partitionInitialConsumed(initialConsumed)
	startupConsumed = append(startupConsumed, carried...)
	for _, m := range startupConsumed {
		totalConsumedMocks[m.Name] = m
		passingTotalConsumedMocks[m.Name] = m
	}
	for _, m := range rearmedConsumed {
		r.logger.Debug("re-arming a per-test mock consumed by the readiness gate before any test fired",
			zap.String("testSetID", testSetID),
			zap.String("mock", m.Name),
			zap.String("kind", string(m.Kind)))
		// Withheld from totalConsumedMocks (so it stays re-armed for serving),
		// but seed a NON-Deleted placeholder in passingTotalConsumedMocks so
		// --remove-unused-mocks / UpdateMocks does not prune this boot/readiness
		// mock from the corpus when its owning test happens to fail or be
		// deselected this run. passingTotalConsumedMocks is never sent to the
		// agent, so this does not undo the re-arm; if the owning test later
		// passes, it overwrites this with the real MockState. Mirrors the
		// non-executed-test preserve block below.
		if _, exists := passingTotalConsumedMocks[m.Name]; !exists {
			passingTotalConsumedMocks[m.Name] = models.MockState{Name: m.Name, Kind: m.Kind}
		}
	}

	// Record the boot-time traffic as the test-set's STARTUP section.
	//
	// These are, by definition, the reusable/session mocks consumed before the
	// first test fired — driver handshakes, auth, connection-pool warm-up. They
	// belong to no single test case, so upsertActualTestMockMapping's per-test
	// window filter can never attribute them (its own comment calls them
	// "session-level traffic that should not be per-test") and until now they
	// were simply absent from mappings.yaml.
	//
	// That absence is invisible on the timestamp path, which reloads them via
	// disk.LoadBefore(firstWindowStart), but fatal on the mapping path, which
	// loads strictly by name. Writing them here is what lets the reader hand
	// them back for every test. The reusable tier is written, the carry-over
	// mocks consumed as the app started, and the re-armed per-test mocks
	// requested before the first test (a boot consumer's fetches: the app's
	// startup traffic, whatever their lifetime; bootBandOf), wherever their
	// answer came at record: a replay serves such a fetch as the app starts,
	// as this one was.
	// A per-test mock re-armed above that was recorded in a test's window (a
	// readiness gate's) belongs to its own test's section, not to startup.
	setupBoot := bootBandOf(rearmedConsumed, models.NewWindowSchedule(recordedTestWindows(testCases)), firstRecordedTestStart(testCases), "")
	setStartupMocks(actualTestMockMappings, append(append([]models.MockState(nil), startupConsumed...), setupBoot...))

	// Snapshot the post-setup consumed-mock baseline. These are the
	// reusable/session mocks (driver handshake, auth, connection pool
	// warm-up) consumed during application bootstrap, before any test
	// fires. Each mock-consistency retry cycle re-runs the passing tests
	// as a fresh forward pass, so its serving pool must START from this
	// baseline — see the rewind at the top of the replay loop below.
	baselineConsumedMocks := make(map[string]models.MockState, len(totalConsumedMocks))
	for k, v := range totalConsumedMocks {
		baselineConsumedMocks[k] = v
	}
	// Persistent union of every MockState consumed across ALL retry cycles.
	// totalConsumedMocks is rewound to baselineConsumedMocks at the start of
	// each retry cycle, so after the loop it no longer holds the full set of
	// consumed mocks. Two post-loop readers need that full union: the
	// run-level Mocks-Consumed telemetry (r.consumedMockNames) and the
	// schema-noise-detection persistence (PersistMockNoise, which reads the
	// learned ReqBodyNoise off each MockState value — including mocks a later
	// test failed on). Fold consumed mocks in here as cycles complete; last
	// write wins, matching the pre-rewind behavior where a re-consumed mock's
	// final-cycle MockState overwrote earlier ones.
	allConsumedAcrossCycles := make(map[string]models.MockState, len(totalConsumedMocks))
	for k, v := range totalConsumedMocks {
		allConsumedAcrossCycles[k] = v
	}
	// Test-set-scoped dependency-assertion bookkeeping.
	//
	// depMissingTests collects the tests that lost a recorded outgoing call so
	// ONE Warn can be emitted per test set at the end, instead of one per test
	// on every run. Keyed by name, and recordUnexercised DELETES on a clean
	// cycle as well as inserting on a dirty one, so a RetryPassing cycle can
	// neither double-count a test nor leave a stale entry for one that
	// recovered — the summary always describes the same cycle the persisted
	// report does. The value is the test's status, so the summary can separate
	// the tests whose response still matched (the silent-green population)
	// from the ones that were demoted.
	//
	// depAssertionInertWarned makes the "you asked for --assert-dependencies
	// but it cannot run here" warning fire once per test set rather than once
	// per test.
	depMissingTests := map[string]models.TestStatus{}
	depAssertionInertWarned := false
	// consumedUnread names the tests that own test mocks and whose consumed
	// mocks could not be read (proven switch): see testMocksUnknown.
	var consumedUnread []string
	// variablesUnjudged names the tests whose captures could not be checked
	// against what their dependency calls carried (proven switch): a
	// keploy-side fault, so the set is not judged (testVariables);
	// variablesWhy is each one's reason. Both are scoped to a test's last
	// run, as consumedUnread is.
	var variablesUnjudged []string
	variablesWhy := map[string]string{}
	variablesWarned := false
	// consumptionUnread names every test, in any cycle and with the switch on or
	// off, for which a read of what it consumed failed: what it consumed is not
	// known, so the mapping write leaves its entry as it is (passingTests).
	var consumptionUnread []string
	// obsoleteShaped names every test whose answer did not match while a
	// recorded mock of its mapping went unconsumed: the shape of a wrong-mock
	// serve, whatever verdict the flags or a hook give it. The mapping write
	// leaves its entry as it is (passingTests).
	var obsoleteShaped []string
	// leftRecordedUnconsumed names every test that left a recorded mock of its
	// mapping unconsumed, whatever its answer. One whose row also reports a
	// call no mock answered keeps its entry as it is too (passedMissShaped).
	var leftRecordedUnconsumed []string
	// The lookup of mock name -> summary, protocol and target from the mock
	// registry (once per test set). `target` is added for the DepResult writer:
	// neither models.MockEntry (the mapping side) nor models.MockState (the
	// consumed side) carries a destination, so the only place a human-meaningful
	// target exists is the loaded *models.Mock. Can legitimately stay empty —
	// r.mockDB may be nil and the lookup's reads swallow their errors — so
	// every consumer must degrade gracefully rather than assume a hit.
	if mockLookup == nil && !r.noAgent {
		// Not built with the mocks above: they were not loaded here. A base
		// path leaves it unread: with nothing mocked, the report has no mock
		// to describe.
		mockLookup = r.readMockLookup(runTestSetCtx, testSetID)
	}

	// A run that started no application (an agent already up, no -c) has
	// not marked the capture losses its tests count from: its first test set
	// marks them here, before its first test (proven switch). Its application
	// is up already, and may have opened a Unix-socket connection that every
	// test rides on (decision 20): what keploy cannot capture at all is
	// counted from the hooks' load, as the agent counts it, not from here.
	if provenswitch.On() && !r.lossMarked {
		if !r.lossRunBaseSet {
			r.lossRunBase, r.lossRunBaseSet = nil, true
		}
		r.markCaptureLosses(runTestSetCtx)
	}

	// Separate replay into regular and streaming buckets. Regular tests can use the
	// iterative replay path from main, while streaming tests are replayed sequentially
	// afterwards so long-lived connections do not block the normal flow.
	var activeTestCases []*models.TestCase
	var streamingTests []streamingTest
	for _, testCase := range testCases {
		if _, ok := selectedTests[testCase.Name]; !ok && len(selectedTests) != 0 {
			continue
		}

		if _, ok := ignoredTests[testCase.Name]; ok {
			testCaseResult := &models.TestResult{
				Kind:         ignoredKind(testCase),
				Name:         testSetID,
				Status:       models.TestStatusIgnored,
				TestCaseID:   testCase.Name,
				TestCasePath: filepath.Join(r.config.Path, testSetID),
				MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
				SkipReason:   r.skipReasonFor(testSetID, testCase.Name),
			}
			loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
			if loopErr != nil {
				utils.LogError(r.logger, loopErr, "failed to insert test case result")
				break
			}
			ignored++
			continue
		}

		if testCase != nil && testCase.Kind == models.HTTP && pkg.IsHTTPStreamingTestCase(testCase) {
			streamingTests = append(streamingTests, newStreamingTest(testCase, expectedTestMockMappings))
			r.logger.Debug("deferring streaming test case",
				zap.String("testcase", testCase.Name),
				zap.String("testset", testSetID))
			continue
		}

		activeTestCases = append(activeTestCases, testCase)
	}

	testsToRun := activeTestCases
	// Each pass's template values: a RetryPassing cycle that skips a test
	// case still gives later ones the value a whole pass gives them.
	tmplPass := newPassValues(tmplRun, activeTestCases)
	// Mocks the agent reports consumed outside their own window (a late server
	// push, a carry-over publish) are kept and mapped by their own window, not
	// by the running test; see carryOverAttribution.
	carryAttr := newCarryOverAttribution(testCases)
	carryAttr.bootListed = len(setupBoot) > 0
	// stoppedEarly records that a cycle exited before every test it intended to
	// run had produced a verdict. The counters cannot express this on their own:
	// `success` is ASSIGNED per cycle while `failure`/`obsolete` accumulate, and
	// `testCasesCount` counts loaded tests including the ignored, the deferred
	// streaming ones and any the selection dropped. Comparing verdicts against
	// the cycle's own testsToRun is the only statement that stays true in all of
	// those cases.
	stoppedEarly := false
	// Tests whose request got no answer, classified off the error in hand by
	// pkg.IsAppNoAnswer.
	var noAnswerTests []string
	noteNoAnswer := func(name string, err error) {
		if pkg.IsAppNoAnswer(err) {
			noAnswerTests = append(noAnswerTests, name)
		}
	}
	// drainBeforeFiling reads, under the proven switch, what a test that
	// ends early (no answer, or one judged on its start) consumed, before its
	// row is filed: its calls are its own, not the next test's, whose drain
	// would read them otherwise. Without the switch an early row files as it
	// always did.
	//
	// answeredInGap and gapReadErr are what the gap before it answered of its
	// calls in flight, read as it opened under exact serving
	// (drainAnsweredInGap), and that read's failure: they are its own too.
	drainBeforeFiling := func(ctx context.Context, name string, answeredInGap []models.MockState, gapReadErr error) {
		if !provenswitch.On() || !r.instrument {
			return
		}
		early, err := r.hookImpl.GetConsumedMocks(ctx)
		if err == nil {
			err = gapReadErr
		}
		if err != nil {
			for _, m := range answeredInGap {
				totalConsumedMocks[m.Name] = m
			}
			consumptionUnread = appendOnce(consumptionUnread, name)
			return
		}
		early = append(answeredInGap, early...)
		for _, m := range early {
			totalConsumedMocks[m.Name] = m
		}
		noteConsumers(consumers, name, early)
	}
	finalTestCaseResults := make(map[string]*models.TestResult)
	itr := 1
	if r.config.RetryPassing {
		itr = 5
	}
	for replay := 0; replay < itr; replay++ {
		// Mock-consistency retry cycles (replay > 0) re-run the tests that
		// passed the previous cycle as a fresh forward pass. Rewind the
		// consumed-mock filter to the post-setup baseline so each re-run
		// test's PER-TEST single-use mocks — consumed via DeleteFilteredMock
		// and flagged Usage=Deleted during the previous cycle — are served
		// again. Without this rewind those mocks stay in totalConsumedMocks
		// as Deleted; the agent's filterOutDeleted then strips them from the
		// retry pool and every test that drives a per-test mock (MongoDB
		// find/update, SQL query, non-idempotent HTTP) fails the retry with
		// match_phase=no_mocks even though it passed the first pass. The
		// window filter alone can't save them: the mock IS in the window,
		// it's the Deleted flag that removes it. Mocks consumed in the prior
		// cycle are folded into allConsumedAcrossCycles first so the post-loop
		// readers (Mocks-Consumed telemetry and PersistMockNoise) stay complete.
		if replay > 0 {
			totalConsumedMocks = r.rewindConsumedForRetryCycle(totalConsumedMocks, baselineConsumedMocks, allConsumedAcrossCycles)
		}
		// Every pass over the set starts each template key at its recorded
		// value, so a cycle's producers capture again from their own answers;
		// a producer the cycle skips gives its last capture at its place.
		tmplPass.start()
		var nextTestsToRun []*models.TestCase
		var currentFailures int
		var currentObsolete int
		var currentSkipped int
		var currentSuccess int
		currentPassingMocks := make(map[string]models.MockState)

		// Labelled so the two type-assertion failure paths inside
		// `switch testCase.Kind` can leave the LOOP. A bare `break` there exits
		// only the switch, and execution falls through to the scoring switch
		// with testPass=false/testResult=nil — counting the same test a second
		// time and then persisting an "internal error: comparison returned no
		// result" row for it, so a test that never ran shows up in the failure
		// total. Their sibling at the simulate-error site is not inside a
		// switch and has always exited the loop; these two differed only by
		// accident of nesting.
	testLoop:
		for idx, testCase := range testsToRun {
			tmplPass.reach(testCase)

			// check if its the last test case running
			if idx == len(testsToRun)-1 && r.isLastTestSet && len(streamingTests) == 0 {
				r.isLastTestCase = true
				testCase.IsLast = true
			}

			if _, ok := selectedTests[testCase.Name]; !ok && len(selectedTests) != 0 {
				currentSkipped++
				continue
			}

			if _, ok := ignoredTests[testCase.Name]; ok {
				testCaseResult := &models.TestResult{
					Kind:         ignoredKind(testCase),
					Name:         testSetID,
					Status:       models.TestStatusIgnored,
					TestCaseID:   testCase.Name,
					TestCasePath: filepath.Join(r.config.Path, testSetID),
					MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
					SkipReason:   r.skipReasonFor(testSetID, testCase.Name),
				}
				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if loopErr != nil {
					utils.LogError(r.logger, loopErr, "failed to insert test case result")
					break
				}
				ignored++
				currentSkipped++
				continue
			}

			// Stop early before the hook and URL mutations if an exit signal is
			// already pending — avoids mutating test cases that will never run.
			select {
			case <-exitLoopChan:
				testSetStatus = getErrStatus()
				exitLoop = true
			default:
			}
			if status, ended := endedPartWay(); ended {
				testSetStatus, exitLoop = status, true
			}
			if exitLoop {
				break
			}

			// Exact serving: this test case opens next, unless it is refused
			// below. When the pool call before it did not name its calls in
			// flight (a retry cycle's first test case, or the one it named did
			// not open), they are named now, before its hooks run, and the pool
			// answers them until it opens (serveExactAhead).
			if len(refusedTests[testCase.Name]) == 0 {
				r.serveExactAhead(runTestSetCtx, r.exact, testCase.Name)
			}

			// Run pre-test mutation hook once per test case (not on retries) to
			// avoid compounding side effects from in-place mutations.
			if replay == 0 {
				if mutator, ok := r.hookImpl.(TestCaseMutator); ok {
					if err := mutator.BeforeTestCaseRun(runTestSetCtx, testCase, testSetID); err != nil {
						utils.LogError(r.logger, err, "BeforeTestCaseRun hook failed; replay continues with test case in current state",
							zap.String("testcase", testCase.Name),
							zap.String("next_step", "check the BeforeTestCaseRun implementation and any external dependencies it uses (e.g. KMS, auth, network)"))
					}
				}
			}

			// replace the request URL's BasePath/origin if provided — gated on
			// replay==0 to prevent the prefix from being joined on again on retries.
			if replay == 0 {
				if err := r.applyBasePath(testCase); err != nil {
					currentFailures++
					testSetStatus = models.TestSetStatusFailed
					if loopErr = r.failUnsentTest(runTestSetCtx, testRunID, testSetID, testCase, err); loopErr != nil {
						break
					}
					continue
				}
			}

			// A placeholder refused when the set loaded: the test case fails
			// with the reason, unsent.
			if why := refusedTests[testCase.Name]; len(why) > 0 {
				currentFailures++
				testSetStatus = models.TestSetStatusFailed
				if loopErr = r.failRefusedTest(runTestSetCtx, testRunID, testSetID, testCase, why); loopErr != nil {
					break
				}
				continue
			}

			var testStatus models.TestStatus
			var testResult *models.Result
			var testPass bool
			var loopErr error
			var captured []models.TemplateCapture
			var templateFailures []models.TemplateFailure
			// boundVariables is what the agent bound in this test (proven
			// switch), for the report; variablesUnchecked, why its captures
			// could not be checked (it then has no verdict).
			var boundVariables []models.VariableBinding
			variablesUnchecked := ""

			reqTime, respTime := testWindowOf(testCase)

			// Per-test names PLUS the test-set's startup names. On this path the
			// agent calls disk.LoadByNames(MockMapping) and loads nothing else,
			// so omitting startup names leaves the app's boot mocks (handshake,
			// auth, pool warm-up) out of the pool for every test — the gap the
			// startup section exists to close. The timestamp path needs no
			// equivalent: it reloads them via disk.LoadBefore(firstWindowStart).
			expectedNames := models.MergeStartupMockNames(expectedTestMockMappings[testCase.Name], startupMockNames)
			// Open this test's capture window BEFORE moving the agent's mock
			// window to it: moving it releases what the test recorded the app
			// being pushed (a Pulsar MESSAGE), and a call the app makes at once
			// in reaction is this test's. Opened after, that miss landed with
			// no window open and was discarded.
			r.openTestErrorCapture(runTestSetCtx, &captureOpened)
			if r.exact != nil {
				err = r.serveExactTest(runTestSetCtx, r.exact, testCase.Name, requestVariables(tmplRun, testCase)...)
			} else {
				err = r.SendMockFilterParamsToAgent(runTestSetCtx, expectedNames, reqTime, respTime, r.consumedForAgent(totalConsumedMocks, perTestRegion), useMappingBased, recordedSetShape{}, requestVariables(tmplRun, testCase)...)
			}
			if err != nil {
				if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), err); ok {
					testSetStatus = resolvedStatus
				} else {
					utils.LogError(r.logger, err, "failed to update mock parameters on agent")
				}
				break
			}
			// Exact serving: what the gap answered of this test case's calls in
			// flight was flagged consumed as it opened. It is read now, before
			// its request is sent, so that a drain after the request (the
			// transport-reset re-send gate, retryResetOnce) holds the request's
			// consumption alone; it is the test case's, read with the rest.
			answeredInGap, gapReadErr := r.drainAnsweredInGap(runTestSetCtx, r.exact, testCase.Name)

			// Host and Port replacements are now handled inside SimulateHTTP/SimulateGRPC via config parameters.
			// This ensures that replaceWith configuration takes precedence over global host/port overrides.

			started := time.Now().UTC()

			// What is sent: the request with this run's template values, on a
			// copy. testCase stays as written.
			sent := toSend(tmplRun, testCase)
			resp, loopErr := r.hookImpl.SimulateRequest(runTestSetCtx, &sent, testSetID)

			// A "connection reset by peer" / unexpected EOF while exchanging
			// the request is, under loaded CI replaying a DOCKER app, dominated
			// by docker's userland proxy (docker-proxy) resetting a freshly
			// accepted host-side connection during connection setup — the app
			// never processed the request, so zero mocks were consumed. That
			// case is a transient transport failure, not a real regression, and
			// keploy would otherwise record a false status_code=0.
			//
			// We re-send ONLY when it is provably safe: the agent reports that
			// THIS request consumed no new mocks. MockManager.GetConsumedMocks
			// is per-call and DRAINING (it returns only what was consumed since
			// the last drain, which the normal per-test drain already emptied),
			// so a non-empty result means this request burned a single-use mock
			// — the gate then refuses to retry, because re-running would
			// re-exercise non-idempotent logic against exhausted mocks and
			// fabricate a verdict. This mirrors the ECONNREFUSED re-send
			// (pkg.SimulateHTTP), just establishing the "nothing irreversible
			// happened" invariant via mock consumption instead of error type.
			//
			// On that refusal path retryResetOnce hands back the mocks its gate
			// DRAINED for this failed request; we fold them into
			// totalConsumedMocks so the next SendMockFilterParamsToAgent's
			// filterOutDeleted still drops those exhausted mocks (the drain only
			// affects keploy-side reporting, never the agent's own serving
			// state — see retryResetOnce).
			if loopErr != nil && pkg.IsTransportConnReset(loopErr) {
				retryResp, retried, drainedConsumed := r.retryResetOnce(runTestSetCtx, &sent, testSetID, loopErr)
				if retried {
					resp, loopErr = retryResp, nil
				} else {
					for _, m := range drainedConsumed {
						totalConsumedMocks[m.Name] = m
					}
				}
			}

			// A gRPC stream the app kept open on keepalives
			// (pkg.ErrStreamHeldOpen): past its recording, every recorded
			// message having come, or short of it (pkg.ErrStreamStalled,
			// whose rest SimulateGRPC took from the recording). What came is
			// compared, with the recorded trailers the app never sent, and
			// the test has no verdict (FailureInfo.StreamHeldOpen, row W1).
			var heldOpen string
			if loopErr != nil && errors.Is(loopErr, pkg.ErrStreamHeldOpen) {
				if g, ok := resp.(*models.GrpcResp); ok && g != nil {
					g.Trailers = sent.GrpcResp.Trailers
					heldOpen, loopErr = loopErr.Error(), nil
				}
			}

			// The test case has its answer, or none: it ends here. Under exact
			// serving the agent serves the pool from now, so what is read next
			// is final, and a call the application makes from here, a late
			// asynchronous call of this test case's included, is the pool's.
			// (Waiting for the test case's calls still in flight comes with
			// the end count, stacked on the proxy's connection counter.)
			exactEndErr := r.serveExactPool(runTestSetCtx, r.exact, nextExactTest(testsToRun[idx+1:], selectedTests, ignoredTests, refusedTests))

			if loopErr != nil {
				if status, ended := endedPartWay(); ended {
					testSetStatus, exitLoop = status, true
					break
				}
			}

			if loopErr != nil && pkg.AnswerBegan(loopErr) && resp != nil {
				// An answer that began (its status and headers, gRPC
				// metadata) and then nothing of its body: its start is judged
				// first, apart from the template and capture path. A start
				// that differs is a judged failure; only one that matches
				// fails closed, not judged, below.
				if testCaseResult, differs := r.startFailedRow(tmplRun, &sent, noAnswerView(tmplRun, &sent), resp, testSetID, started); differs {
					sayStartJudged(r.logger, testCase.Name, loopErr)
					noteNoAnswer(testCase.Name, loopErr)
					drainBeforeFiling(runTestSetCtx, testCase.Name, answeredInGap, gapReadErr)
					currentFailures++
					testSetStatus = models.TestSetStatusFailed
					r.attachMockErrors(runTestSetCtx, testSetID, testCase.Name, testCaseResult)
					loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
					if loopErr != nil {
						utils.LogError(r.logger, loopErr, "failed to insert test case result for an answer judged on its start")
						break
					}
					continue
				}
			}

			if loopErr != nil {
				sayNoAnswer(r.logger, testCase.Name, loopErr, "failed to simulate request")
				noteNoAnswer(testCase.Name, loopErr)
				drainBeforeFiling(runTestSetCtx, testCase.Name, answeredInGap, gapReadErr)
				currentFailures++
				testSetStatus = models.TestSetStatusFailed
				testCaseResult := r.CreateFailedTestResult(noAnswerView(tmplRun, &sent), testSetID, started, loopErr)
				markAnswerStalled(testCaseResult, loopErr)
				// Finalize the capture window even on this early exit, so a miss
				// during this (failed) test attaches here and isn't carried to
				// the next test or lost.
				r.attachMockErrors(runTestSetCtx, testSetID, testCase.Name, testCaseResult)
				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if loopErr != nil {
					utils.LogError(r.logger, loopErr, "failed to insert test case result for simulation error")
					break
				}
				continue
			}

			// instrumentConsumedFetchErr captures whether THIS test's per-test
			// consumed-mock fetch in instrument mode succeeded. Read later by
			// the per-test perTestConsumedKnown signal so a failed instrument
			// fetch is treated the same as a failed non-instrument fetch
			// (skip MockMismatches / MatchedCalls rather than emit stale data).
			var instrumentConsumedFetchErr error
			if r.instrument {
				consumedMocks, err = r.hookImpl.GetConsumedMocks(runTestSetCtx)
				if err == nil && exactEndErr != nil {
					err = exactEndErr
				}
				if err == nil && gapReadErr != nil {
					err = gapReadErr
				}
				if err == nil && len(answeredInGap) > 0 {
					consumedMocks = append(answeredInGap, consumedMocks...)
				}
				instrumentConsumedFetchErr = err
				if err != nil {
					consumptionUnread = appendOnce(consumptionUnread, testCase.Name)
					if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), err); ok {
						testSetStatus = resolvedStatus
						exitLoop = true
					} else {
						utils.LogError(r.logger, err, "failed to get consumed filtered mocks")
					}
					// On fetch failure, consumedMocks may carry stale/partial
					// data from the previous test. Clear it so downstream uses
					// in this iteration (logging, mockNames, upsertActualTest-
					// MockMapping) and the next iteration's filter params
					// don't attribute the previous test's data to this one.
					consumedMocks = nil
				}
				r.logger.Debug("consumed mocks after test case simulation",
					zap.String("testSetID", testSetID),
					zap.String("testCaseID", testCase.Name),
					zap.Int("count", len(consumedMocks)),
					zap.Any("mocks", consumedMocks),
					zap.Bool("fetchOk", instrumentConsumedFetchErr == nil))
				// totalConsumedMocks aggregation is implicit-gated: on fetch
				// failure consumedMocks was nil'd above, so this loop is a
				// no-op and stale data can't leak into the next iteration's
				// filter params (SendMockFilterParamsToAgent).
				for _, m := range consumedMocks {
					totalConsumedMocks[m.Name] = m
				}
				noteConsumers(consumers, testCase.Name, consumedMocks)
			}

			r.logger.Debug("test case kind", zap.String("kind", string(testCase.Kind)), zap.String("testcase", testCase.Name), zap.String("testset", testSetID))

			mockNames := make([]string, 0, len(consumedMocks))
			for _, m := range consumedMocks {
				mockNames = append(mockNames, m.Name)
			}

			expectedMocks, hasExpectedMocks := expectedTestMockMappings[testCase.Name]

			// Compute non-DNS expected and consumed name slices once; reused for subset check and mismatch reporting.
			// Only PER-TEST mocks participate in the consumed-vs-expected
			// assertion. DNS (non-deterministic resolution order),
			// connection failures (nothing consumes one at replay yet) and
			// reusable/startup-tier mocks (session / connection / config,
			// recorded once at app boot and shared across tests) stay in the
			// mapping but are excluded here, from both sides: including them
			// would falsely demote tests to OBSOLETE.
			//
			// eligibleExpectedEntries is the SHARED filter — literally the
			// same call buildDepResults makes. Both the verdict signal
			// (mockSetMismatch, via filteredExpectedNames) and the honesty
			// signal (noEligibleDeps, below) are derived from its result, so
			// they cannot drift from the rows the writer emits. It used to be
			// written out inline here and again in the writer; see the
			// function's doc comment for what a one-sided widening of those
			// two copies did to the report.
			eligibleExpected := eligibleExpectedEntries(expectedMocks, mockKindByName, reusableMockNames)
			filteredExpectedNames := make([]string, 0, len(eligibleExpected))
			for _, m := range eligibleExpected {
				filteredExpectedNames = append(filteredExpectedNames, m.Name)
			}

			// consumedOutsideAssertion is the consumed-side twin of
			// eligibleExpectedEntries (and the filter buildDepResults uses), so
			// the two sides exclude the same kinds.
			filteredMockNames := make([]string, 0, len(consumedMocks))
			for _, m := range consumedMocks {
				if consumedOutsideAssertion(m) {
					continue
				}
				filteredMockNames = append(filteredMockNames, m.Name)
			}

			// depAssertionValid: the ONE predicate under which an
			// expected-vs-consumed comparison is a valid per-test assertion.
			// It gates the verdict signal (mockSetMismatch) AND the DepResult
			// rows that explain it, so the two cannot diverge.
			//
			// Every conjunct is load-bearing:
			//   - r.instrument: SendMockFilterParamsToAgent returns early when
			//     it is false, so the per-test mapping is NEVER ARMED and the
			//     agent serves from the whole pool. "Which mock got consumed
			//     for this request" is then decided across all tests, so the
			//     comparison is not attributable to one test at all.
			//   - useMappingBased && isMappingEnabled: without them
			//     determineMockingStrategy hands back defaultMappings and there
			//     is no per-test expectation to compare against.
			//   - hasExpectedMocks: nothing recorded for this test.
			//   - instrumentConsumedFetchErr == nil: the fetch failed and
			//     consumedMocks was cleared to nil above, which would
			//     deterministically compute mockSetMismatch=true (empty subset
			//     of any non-empty expected set) and falsely mark the test
			//     OBSOLETE — and, on the rows side, report every dependency as
			//     missing — because of an unrelated transport error.
			depAssertionValid := r.instrument && useMappingBased && isMappingEnabled && hasExpectedMocks && instrumentConsumedFetchErr == nil
			// Under the proven switch a test that owns test mocks has no verdict
			// when what it consumed could not be read: whether it made every
			// recorded call is unknown.
			testMocksUnread := testMocksUnknown(r.instrument && useMappingBased && isMappingEnabled, len(filteredExpectedNames), instrumentConsumedFetchErr)
			if testMocksUnread {
				consumedUnread = appendOnce(consumedUnread, testCase.Name)
			} else {
				// Scoped to the test's last run, whose row is the one kept
				// (finalTestCaseResults): a --retry-passing cycle that read
				// what it consumed clears an earlier cycle's unread read.
				consumedUnread = slices.DeleteFunc(consumedUnread, func(n string) bool { return n == testCase.Name })
			}

			mockSetMismatch := false
			if depAssertionValid {
				// Compare only per-test mocks (DNS + reusable/startup tiers
				// excluded above) since those are the only mocks
				// deterministically tied to a single test.
				mockSetMismatch = !isMockSubset(filteredMockNames, filteredExpectedNames)
			}
			// noEligibleDeps: the recording DOES map dependencies to this
			// test and the filter above removed every one of them, so
			// buildDepResults has nothing to assert and reports NOT-CHECKED
			// (see its eligibility guard). That is the ordinary outcome for a
			// recording whose mocks carry no per-test tier tag — untagged
			// HTTP/Postgres/MySQL egress is classified session-tier by
			// models.Mock.DeriveLifetime — so without a warning the user gets
			// a report full of `dependencies_checked: false` and no statement
			// of why.
			//
			// Computed from the SAME two lists the assertion itself uses
			// (expectedMocks, filteredExpectedNames), so the warning cannot
			// claim something buildDepResults disagrees with.
			noEligibleDeps := len(expectedMocks) > 0 && len(filteredExpectedNames) == 0

			// One WARN per test set when the knob was asked for but cannot be
			// honoured. Without it --assert-dependencies in CI against a
			// legacy (unmapped) test set, with test.disableMapping set, or
			// against a recording with no per-test-tier dependency at all,
			// produces a green run and no indication the assertion never ran
			// — the same silent-green class the slice exists to close.
			//
			// Called per TEST CASE (the latch keeps it one line per test set)
			// because noEligibleDeps is a property of this test's own mapped
			// entries, not of the test set.
			//
			// deferredStreaming is false here: this loop only ever sees the
			// non-streaming bucket. The streaming tests were split out above
			// and get their own latched call in Phase 2.
			//
			// instrumentConsumedFetchErr != nil is passed as its OWN reason
			// rather than being left to fall through to noEligibleDeps. A
			// failed fetch nils consumedMocks, which is a transport error the
			// operator can act on; blaming the recording's tier for it sent
			// them to re-tag a recording that was never the problem, and a
			// fetch failure on a test set WITH eligible dependencies used to
			// produce no warning at all.
			r.warnDependencyAssertionInert(testSetID, useMappingBased, isMappingEnabled, instrumentConsumedFetchErr != nil, false, noEligibleDeps, &depAssertionInertWarned)

			// A mock-set mismatch normally means "re-record", so the response
			// diff is suppressed as noise. With --assert-dependencies the test
			// is about to be marked FAILED instead of demoted to OBSOLETE, so
			// suppressing its diffs would hand the user a red test with no
			// explanation. Knob off (the default) is byte-identical to before.
			// Under the proven switch strict failure is always on, so a test
			// whose mock set diverged is never demoted: its response diff is the
			// explanation of its FAILED verdict and must be printed
			// (assertTestMocks is on under the switch). --strict-failure keeps
			// it FAILED too (resolveTestOutcome), so its diff is printed.
			// Every failed answer's diff is printed: a wrong answer demoted to
			// OBSOLETE is not judged and fails the run (FailureInfo.MappingStale),
			// so what differed is what its user reads before re-recording.
			emitFailureLogs := true

			// This test's misses are read now, once, with its response in, so
			// the verdict can hold a miss. Every result written for this test
			// below attaches this read (attachMisses): a second read would find
			// the agent's channel drained. Under the proven switch a miss fails
			// the test, so the result line, the counters and the retry list all
			// hold it (earlyMissed); with the switch off a miss keeps a test
			// whose mock set diverged from being demoted to OBSOLETE (missed):
			// it got no answer to a call, which is no sign its recording is out
			// of date, and the demotion would turn its failure into a green run.
			read := r.readMockErrors(runTestSetCtx)
			missed := dataPlaneMisses(read.calls)
			earlyMissed := 0
			if provenswitch.On() {
				earlyMissed = missed
			}
			attachMisses := func(res *models.TestResult) {
				r.attachReadMockErrors(read, testSetID, testCase.Name, res)
				if provenswitch.On() {
					missCategory(res)
				}
			}
			// Under the proven switch a test keploy's agent lost a call of (or
			// of a test before it) fails, as one with a miss does: read with
			// its misses, before the comparison, so the verdict holds it.
			lostCapture := read.failsOnCaptureLoss()
			// The switch fails this test whatever its answer says when a call
			// of it got no recorded answer, was not captured, or a recorded
			// call of it was not made: a comparison that matches prints no
			// pass line for it.
			failedOnItsOwn := earlyMissed > 0 || lostCapture || (provenswitch.On() && mockSetMismatch && r.assertTestMocks())
			// A matching answer does not pass a test the verdict fails anyway:
			// one the switch fails on its own, or one --assert-dependencies (or
			// a strict mock noise) fails for its diverged mock set with the
			// switch off. Its comparison prints no pass line; it is compared as
			// before.
			noPassLine := failedOnItsOwn || resolveTestOutcome(true, mockSetMismatch, r.config.Test.NoiseStrict(), r.assertTestMocks(), r.strictFailure()).Status != models.TestStatusPassed

			switch testCase.Kind {
			case models.HTTP:
				httpResp, ok := resp.(*models.HTTPResp)
				if !ok {
					r.logger.Error("invalid response type for HTTP test case")
					currentFailures++
					testSetStatus = models.TestSetStatusFailed
					testCaseResult := r.CreateFailedTestResult(noAnswerView(tmplRun, &sent), testSetID, started, errors.New("invalid response type for HTTP test case"))
					attachMisses(testCaseResult)
					loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
					if loopErr != nil {
						utils.LogError(r.logger, loopErr, fmt.Sprintf("failed to insert test case result for type assertion error in %s test case", testCase.Kind))
						break testLoop
					}
					continue
				}
				// The answer's captures first, then the rest of the expected
				// response at the run's values; then compare as written. A
				// variable place the answer does not satisfy fails the test
				// case whatever the comparison says.
				resolved := r.resolveExpected(tmplRun, &tmplCheck, &sent, httpResp, testSetID)
				tmplPass.captured(testCase, resolved.captures)
				captured = reportCaptures(resolved.captures)
				templateFailures = reportFailures(resolved.failures)
				var varFailures []models.TemplateFailure
				var unjudged string
				boundVariables, varFailures, unjudged = r.testVariables(runTestSetCtx, tmplRun, testCase, resolved.captures, &variablesWarned)
				templateFailures = append(templateFailures, varFailures...)
				if unjudged != "" {
					variablesUnjudged = appendOnce(variablesUnjudged, testCase.Name)
					variablesWhy[testCase.Name] = unjudged
					variablesUnchecked = unjudged
				} else {
					// A --retry-passing-test cycle that checked it clears an
					// earlier cycle's unchecked run (its row is the last's).
					variablesUnjudged = slices.DeleteFunc(variablesUnjudged, func(n string) bool { return n == testCase.Name })
					delete(variablesWhy, testCase.Name)
				}
				opts := resolved.opts
				if noPassLine || len(varFailures) > 0 {
					// The test case has failed on its own: no pass line.
					opts = append(opts, httpMatcher.WithoutPassLine())
				}
				if len(templateFailures) > 0 || failedOnItsOwn {
					// The test case has failed on its own: the additive-schema
					// auto-pass (compareHTTPRespForReplay) never passes it, and
					// its comparison rows stay as compared.
					testPass, testResult = r.compareHTTPResp(resolved.expected, httpResp, testSetID, emitFailureLogs, opts...)
				} else {
					testPass, testResult = r.compareHTTPRespForReplay(resolved.expected, httpResp, testSetID, emitFailureLogs, opts...)
				}
				if len(templateFailures) > 0 {
					testPass = false
					markTemplateFailures(testResult)
				}

			case models.GRPC_EXPORT:
				grpcResp, ok := resp.(*models.GrpcResp)
				if !ok {
					r.logger.Error("invalid response type for gRPC test case")
					currentFailures++
					testSetStatus = models.TestSetStatusFailed
					testCaseResult := r.CreateFailedTestResult(noAnswerView(tmplRun, &sent), testSetID, started, errors.New("invalid response type for gRPC test case"))
					attachMisses(testCaseResult)
					loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
					if loopErr != nil {
						utils.LogError(r.logger, loopErr, "failed to insert test case result for type assertion error")
						break testLoop
					}
					continue
				}

				respCopy := *grpcResp
				// The expected response with the run's template values, on a
				// copy: gRPC responses never capture.
				grpcExpected := noAnswerView(tmplRun, &sent)

				if r.config.Test.ProtoFile != "" || r.config.Test.ProtoDir != "" || len(r.config.Test.ProtoInclude) > 0 {
					// get the :path header from the request
					method, ok := grpcExpected.GrpcReq.Headers.PseudoHeaders[":path"]
					if !ok {
						utils.LogError(r.logger, nil, "failed to get :path header from the request, cannot convert grpc response to json")
						goto compareResp
					}

					pc := models.ProtoConfig{
						ProtoFile:    r.config.Test.ProtoFile,
						ProtoDir:     r.config.Test.ProtoDir,
						ProtoInclude: r.config.Test.ProtoInclude,
						RequestURI:   method,
					}

					// get the proto message descriptor
					md, files, err := utils.GetProtoMessageDescriptor(context.Background(), r.logger, pc)
					if err != nil {
						utils.LogError(r.logger, err, "failed to get proto message descriptor, cannot convert grpc response to json")
						goto compareResp
					}

					// convert both actual and expected using the same path (protoscope-text -> wire -> json)
					actJSON, actOK := utils.ProtoTextToJSON(md, files, respCopy.Body.DecodedData, r.logger)
					testJSON, testOK := utils.ProtoTextToJSON(md, files, grpcExpected.GrpcResp.Body.DecodedData, r.logger)

					if actOK && testOK {
						respCopy.Body.DecodedData = string(actJSON)
						grpcExpected.GrpcResp.Body.DecodedData = string(testJSON)
					}
				}

			compareResp:
				// Each use place is asserted on its own, against the answer
				// as it came, whatever the comparison and its noise say. It
				// is read first, so a test case it fails never prints the
				// comparison's pass line.
				templateFailures = reportFailures(r.grpcTemplateFailures(tmplRun, &tmplCheck, &sent, grpcResp, testSetID))
				testPass, testResult = r.compareGRPCResp(grpcExpected, &respCopy, testSetID, emitFailureLogs, len(templateFailures) > 0 || noPassLine)
				if len(templateFailures) > 0 {
					testPass = false
					markTemplateFailures(testResult)
				}
			}

			tcReqTime, tcRespTime := recordReqResTimestamps(testCase)
			carryAttr.mapConsumed(actualTestMockMappings, testCase.Name, tcReqTime, tcRespTime, consumedMocks)

			// log the consumed mocks during the test run of the test case for test set
			r.logger.Debug("consumed mocks for test case",
				zap.String("testSetID", testSetID),
				zap.String("testCaseID", testCase.Name),
				zap.Strings("mockNames", mockNames),
				zap.Any("mocks", consumedMocks))

			// THE PER-TEST VERDICT, in one call.
			//
			// resolveTestOutcome takes the PRE-PROMOTION response result and
			// returns everything that follows from it: the persisted status,
			// whether the test set goes red, whether the mismatch is recorded
			// for the end-of-run report, and which log line explains it.
			//
			// It is a seam on purpose. The promotions used to be inline
			// `case testPass && <knob>:` arms of a switch inside this
			// 3000-line function, and a reviewer demonstrated that replacing
			// the --assert-dependencies arm with `testPass && false` left the
			// entire five-package suite green — i.e. the knob's only reason to
			// exist was unguarded. Every arm is now a table row in
			// TestResolveTestOutcome.
			//
			// The three knobs it weighs:
			//   - SchemaNoiseStrict: an expected mock was REJECTED because a
			//     non-noise request-body field drifted.
			//   - AssertDependencies: an expected per-test mock was not
			//     observed during this test's window, i.e. the app stopped
			//     making an outgoing call the recording says it makes. The
			//     response can still match — a deterministic dependency, a
			//     cached value, a swallowed error — so the response check
			//     alone cannot catch it, and today such a test passes. (One
			//     whose response failed too is demoted to OBSOLETE, which
			//     leaves it and its set not judged: false-pass row 0 of
			//     keploy-consumer-design-v2.md §5, closed.)
			//     DEFAULT OFF: every existing suite with a drifting mock pool
			//     would flip red on upgrade otherwise. The DepResult rows are
			//     written and rendered either way, so with the knob off the
			//     missing dependency is still visible in the report / JUnit /
			//     --format json — only the verdict differs.
			//   - StrictFailure: the pre-existing veto of the OBSOLETE
			//     demotion for a response-failing test.
			outcome := resolveTestOutcome(testPass, mockSetMismatch, r.config.Test.NoiseStrict(), r.assertTestMocks(), r.strictFailure())
			if mockSetMismatch {
				leftRecordedUnconsumed = appendOnce(leftRecordedUnconsumed, testCase.Name)
				if !testPass {
					obsoleteShaped = appendOnce(obsoleteShaped, testCase.Name)
				}
			}
			// A template failure is never demoted to OBSOLETE.
			outcome = keepTemplateFailure(outcome, len(templateFailures) > 0)
			// Under the proven switch a call of this test that got no recorded
			// answer fails it (earlyMissed, read before the comparison).
			if earlyMissed > 0 {
				outcome = failOnMisses(outcome)
				r.logger.Error("calls this test made got no recorded answer; failing testcase (KEPLOY_PROVEN=1); the report lists each under unmatched_calls",
					zap.String("testcase", testCase.Name),
					zap.String("testset", testSetID),
					zap.Int("unmatchedCalls", earlyMissed))
			} else if missed > 0 && outcome.Status == models.TestStatusObsolete {
				outcome = failOnMisses(outcome)
				r.logger.Error("mock mapping mismatch detected; marking testcase as FAILED, not obsolete: calls it made got no recorded answer, and the report lists each under unmatched_calls",
					zap.String("testcase", testCase.Name),
					zap.String("testset", testSetID),
					zap.String("reason", vetoUnmatchedCallSwitchOff),
					zap.Int("unmatchedCalls", missed))
			}
			// A test that lost a call fails in this cycle, as one with a miss
			// does, so the result line, the counters and the retry list hold
			// it: a --retry-passing-test cycle runs only the tests that
			// passed, and one that ran it again and lost nothing passed it.
			// Said whatever else failed it, as a miss is: a lost call may be
			// why its answer is wrong.
			if lostCapture {
				outcome = failOnCaptureLoss(outcome)
				r.logger.Error("keploy's agent could not capture every call while this test or a test before it ran; failing testcase (KEPLOY_PROVEN=1); the report says what under capture_incomplete",
					zap.String("testcase", testCase.Name),
					zap.String("testset", testSetID))
			}
			switch outcome.Log {
			case mismatchLogSchemaNoiseReject:
				r.logger.Error("strict schema-noise: expected mock was rejected (non-noise request-body drift); failing testcase even though the response matched",
					zap.String("testcase", testCase.Name),
					zap.String("testset", testSetID),
					zap.Strings("expectedMocks", filteredExpectedNames),
					zap.Strings("actualMocks", filteredMockNames))
			case mismatchLogDependencyReject:
				if provenswitch.On() {
					// The switch rejects it, whatever the flags say.
					r.logger.Error("a recorded call of this test was not made during its window; failing testcase even though the response matched",
						zap.String("testcase", testCase.Name),
						zap.String("testset", testSetID),
						zap.String("reason", vetoFlagName(true, false)),
						zap.Strings("expectedMocks", filteredExpectedNames),
						zap.Strings("actualMocks", filteredMockNames))
					break
				}
				r.logger.Error("assert-dependencies: an expected per-test dependency was not observed during this test's window; failing testcase even though the response matched",
					zap.String("testcase", testCase.Name),
					zap.String("testset", testSetID),
					zap.Strings("expectedMocks", filteredExpectedNames),
					zap.Strings("actualMocks", filteredMockNames))
			case mismatchLogIgnoredResponseMatched:
				r.logger.Debug("mock mapping mismatch ignored because testcase passed",
					zap.String("testcase", testCase.Name),
					zap.String("testset", testSetID),
					zap.Strings("expectedMocks", filteredExpectedNames),
					zap.Strings("actualMocks", filteredMockNames))
			case mismatchLogVetoedFailure:
				r.logger.Error("mock mapping mismatch detected; marking testcase as FAILED",
					zap.String("testcase", testCase.Name),
					zap.String("testset", testSetID),
					zap.String("reason", outcome.VetoFlags),
					zap.Strings("expectedMocks", filteredExpectedNames),
					zap.Strings("actualMocks", filteredMockNames))
			case mismatchLogObsolete:
				r.logger.Error("mock mapping mismatch detected; marking testcase as obsolete. Re-record the test case, or, if its mapping is stale, remap it with --disable-mapping --update-test-mapping (which serves it its recorded window); --update-test-mapping alone keeps its recorded mapping, as it updates only a passing test's",
					zap.String("testcase", testCase.Name),
					zap.String("testset", testSetID),
					zap.Strings("expectedMocks", filteredExpectedNames),
					zap.Strings("actualMocks", filteredMockNames))
			case mismatchLogNone:
			}
			if outcome.RecordMismatch {
				r.mockMismatchFailures.AddFailure(testSetID, testCase.Name, filteredExpectedNames, filteredMockNames)
			}
			// Fold the promotions back into testPass so the "result" line and
			// every later reader see the resolved verdict, exactly as they did
			// when the flips were inline.
			testPass = outcome.Status == models.TestStatusPassed

			if !testPass {
				r.logger.Info("result", zap.String("testcase id", models.HighlightFailingString(testCase.Name)), zap.String("testset id", models.HighlightFailingString(testSetID)), zap.String("passed", models.HighlightFailingString(testPass)))
			} else {
				r.logger.Info("result", zap.String("testcase id", models.HighlightPassingString(testCase.Name)), zap.String("testset id", models.HighlightPassingString(testSetID)), zap.String("passed", models.HighlightPassingString(testPass)))
			}
			testStatus = outcome.Status
			switch testStatus {
			case models.TestStatusPassed:
				currentSuccess++
				nextTestsToRun = append(nextTestsToRun, testCase)
				for _, m := range consumedMocks {
					passingTotalConsumedMocks[m.Name] = m
				}
			case models.TestStatusObsolete:
				currentObsolete++
			default:
				currentFailures++
			}
			carryAttr.keepWhateverTheVerdict(passingTotalConsumedMocks, testCase.Name, consumedMocks)
			if outcome.FailsTestSet {
				testSetStatus = models.TestSetStatusFailed
			}

			if testResult != nil {
				var testCaseResult *models.TestResult

				switch testCase.Kind {
				case models.HTTP:
					httpResp := resp.(*models.HTTPResp)

					testCaseResult = &models.TestResult{
						Kind:       models.HTTP,
						Name:       testSetID,
						Status:     testStatus,
						Started:    started.Unix(),
						Completed:  time.Now().UTC().Unix(),
						TestCaseID: testCase.Name,
						// The request as it was sent: each placeholder at
						// the value it carried.
						Req: models.HTTPReq{
							Method:     sent.HTTPReq.Method,
							ProtoMajor: sent.HTTPReq.ProtoMajor,
							ProtoMinor: sent.HTTPReq.ProtoMinor,
							URL:        sent.HTTPReq.URL,
							URLParams:  sent.HTTPReq.URLParams,
							Header:     sent.HTTPReq.Header,
							Body:       sent.HTTPReq.Body,
							Binary:     sent.HTTPReq.Binary,
							Form:       sent.HTTPReq.Form,
							Timestamp:  sent.HTTPReq.Timestamp,

							HeaderLineLengths: sent.HTTPReq.HeaderLineLengths,
						},
						Res:          *httpResp,
						TestCasePath: filepath.Join(r.config.Path, testSetID),
						MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
						Noise:        testCase.Noise,
						Result:       *testResult,
						TimeTaken:    time.Since(started).String(),
						Captured:     captured,
						Variables:    boundVariables,

						TemplateFailures: templateFailures,
					}
				case models.GRPC_EXPORT:
					grpcResp := resp.(*models.GrpcResp)

					testCaseResult = &models.TestResult{
						Kind:         models.GRPC_EXPORT,
						Name:         testSetID,
						Status:       testStatus,
						Started:      started.Unix(),
						Completed:    time.Now().UTC().Unix(),
						TestCaseID:   testCase.Name,
						GrpcReq:      sent.GrpcReq,
						GrpcRes:      *grpcResp,
						TestCasePath: filepath.Join(r.config.Path, testSetID),
						MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
						Noise:        testCase.Noise,
						Result:       *testResult,
						TimeTaken:    time.Since(started).String(),

						TemplateFailures: templateFailures,
					}
				}

				if testCaseResult != nil {
					if testStatus == models.TestStatusFailed && testResult.FailureInfo.Risk != models.None {
						testCaseResult.FailureInfo.Risk = testResult.FailureInfo.Risk
						testCaseResult.FailureInfo.Category = testResult.FailureInfo.Category
						testCaseResult.FailureInfo.Assessment = testResult.FailureInfo.Assessment
					}
					// Per-test consumed-mock fetch + failure-diagnostic population.
					// Two distinct outputs are computed from the per-test
					// consumed-mock list below:
					//   (a) MatchedCalls / UnmatchedCalls on FailureInfo —
					//       populated only on failed/obsolete tests
					//       (UnmatchedCalls is independent of the consumed
					//       set; MatchedCalls gates on perTestConsumedKnown).
					//   (b) TestResult.MockMismatches — populated on EVERY
					//       test (pass or fail) when consumed data is known.
					//
					// In non-instrument mode (k8s-proxy / remote agent) the
					// loop-scoped `consumedMocks` is only refreshed inside the
					// `if r.instrument` branch after simulation — for non-
					// instrument it stays at whatever the initial setup set,
					// which is stale for every test except the first. Fetch
					// the per-test consumed-mock set into a PER-TEST LOCAL
					// (perTestConsumed) rather than overwriting consumedMocks,
					// so the value doesn't leak into the next iteration's
					// upsertActualTestMockMapping / mockNames computations.
					// One fetch services both outputs (a) and (b).
					//
					// On fetch error we set perTestConsumedKnown=false and
					// skip both MatchedCalls population AND MockMismatches
					// for this test (rather than falling back to the stale
					// loop-scoped consumedMocks, which would emit misleading
					// data attributed to the wrong test case).
					// perTestConsumedKnown tracks whether consumed-mock data for
					// THIS test is reliable. Instrument mode: only when the
					// per-test fetch upstream (in the `if r.instrument` branch
					// after simulation) succeeded — a failed instrument fetch
					// leaves consumedMocks at stale/partial state, same as
					// non-instrument's stale loop-scope problem. Non-instrument
					// mode: only when this block's per-test fetch succeeds.
					// MatchedCalls / MockMismatches gate on this signal so
					// stale data isn't attributed to the wrong test case.
					// A base path has no fetch to make: it mocks nothing, so
					// there is no consumed mock to know of.
					perTestConsumed := consumedMocks
					perTestConsumedKnown := r.instrument && instrumentConsumedFetchErr == nil
					if !r.instrument && !r.noAgent {
						if fetched, fetchErr := r.hookImpl.GetConsumedMocks(runTestSetCtx); fetchErr == nil {
							perTestConsumed = fetched
							perTestConsumedKnown = true
						} else {
							// Mirror the instrument-mode skip: leaves
							// perTestConsumedKnown false so downstream skips
							// MatchedCalls AND MockMismatches (any consumed-
							// mock-derived field) rather than attribute stale
							// data. UnmatchedCalls is unaffected — independent
							// sources (mockMismatchFailures channel + GetMock-
							// Errors) still populate for failed/obsolete tests.
							r.logger.Debug("non-instrument GetConsumedMocks failed; skipping all consumed-mock-derived fields (MatchedCalls + MockMismatches) for this test",
								zap.String("testSetID", testSetID),
								zap.String("testCaseID", testCase.Name),
								zap.Error(fetchErr))
						}
					}
					if testStatus == models.TestStatusFailed || testStatus == models.TestStatusObsolete {
						// MatchedCalls is built from perTestConsumed and is
						// only meaningful when that data is known for THIS
						// test. Skip if not — stale data would attribute the
						// previous test's matched mocks to this one.
						if perTestConsumedKnown {
							for _, m := range perTestConsumed {
								if m.Kind != models.DNS {
									info := mockLookup[m.Name]
									protocol := info.protocol
									if protocol == "" {
										protocol = string(m.Kind)
									}
									testCaseResult.FailureInfo.MatchedCalls = append(testCaseResult.FailureInfo.MatchedCalls, models.MatchedCall{
										MockName: m.Name,
										Protocol: protocol,
										Summary:  info.summary,
									})
								}
							}
						}
					}
					// UnmatchedCalls is finalized for EVERY test, not just
					// failed/obsolete ones: (1) a miss during an otherwise-passing
					// test must still surface; (2) the per-test capture window
					// opened by openTestErrorCapture must be drained-and-closed
					// each iteration so a miss can't carry over to the next test.
					// attachMockErrors (GetMockErrors -> result + summary store) is
					// the single source of unmatched outgoing calls across all
					// transports — native and k8s alike reach the agent over HTTP,
					// so the agent's error channel is consumed only inside the
					// agent's own drain goroutine, never by the replayer.
					if testMocksUnread {
						// No verdict: its row says so, before any hook reads it.
						testCaseResult.FailureInfo.TestMocksUnread = true
					}
					if variablesUnchecked != "" {
						// No verdict either: what it captured was not checked.
						testCaseResult.FailureInfo.VariablesUnchecked = variablesUnchecked
					}
					if heldOpen != "" {
						// No verdict: the app kept its stream open past the
						// recording.
						testCaseResult.FailureInfo.StreamHeldOpen = heldOpen
					}
					attachMisses(testCaseResult)
					// Build the {expected, actual} mock set for THIS test case.
					// See buildExpectedMockInfos / buildActualMockInfos at the
					// bottom of this file for DNS-filter + perTestConsumed-known
					// semantics. Both extracted as helpers for unit testability.
					// Startup names are dropped from BOTH sides, matching the
					// runner path (checkMockMismatches) and the existing DNS
					// carve-out. They belong to the test SET, so neither their
					// presence nor their absence in one test is a mismatch: a step
					// that re-handshakes would otherwise report an unexpected
					// consumption, and one that did not would report a phantom
					// missing expectation for any name that appears in both
					// sections. Report-only — MockMismatches does not drive the
					// pass/fail or obsolescence verdict.
					expectedMockInfos := dropStartupMockInfos(buildExpectedMockInfos(expectedMocks, mockKindByName), startupMockNames)
					actualMockInfos := dropStartupMockInfos(buildActualMockInfos(perTestConsumed, perTestConsumedKnown), startupMockNames)

					// FIRST WRITER of models.Result.DepResult
					// (keploy-consumer-design-v2.md §2, §7 slice 4). Written
					// for EVERY test that reaches here — passed, failed or
					// obsolete — so a missing dependency is visible in the
					// report, JUnit and --format json regardless of the
					// verdict.
					//
					// Gated on the SAME depAssertionValid predicate as
					// mockSetMismatch: the rows are only a valid per-test
					// assertion when the per-test mock mapping was actually
					// ARMED on the agent, which is instrument mode only. Under
					// that predicate perTestConsumed == consumedMocks and
					// perTestConsumedKnown is implied, so the rows and the
					// verdict signal are computed from identical inputs.
					//
					// The dependencies that WERE observed collapse into a
					// plain count (Result.DepsConsumed) in every mode:
					// persistence is decoupled from the verdict knob on
					// purpose, so turning the CI gate on cannot multiply the
					// size of every report it writes. Result.DepsChecked is
					// written unconditionally under the predicate, which is
					// what makes "the assertion ran and found nothing"
					// expressible on disk and distinguishable from "the
					// assertion never ran" — for two short keys instead of the
					// ~224-byte aggregate row an earlier revision persisted per
					// test, on every report, to encode the same one bit.
					//
					// Rebuilt on every RetryPassing cycle: the last cycle's
					// rows land in finalTestCaseResults, overwriting earlier
					// ones, so no stale row survives a mock rewind.
					//
					// This is a read-only projection of bookkeeping the replay
					// loop already computes — no new capture path, and mock
					// matching / serving is untouched.
					depAssert := buildDepResults(expectedMocks, perTestConsumed, depAssertionValid && perTestConsumedKnown, reusableMockNames, mockKindByName, mockLookup)
					missingDepNames, depLevel := attachDepResults(testCaseResult, testStatus, depAssert, r.assertTestMocks())
					recordUnexercised(depMissingTests, testCase.Name, testStatus, depLevel)
					switch depLevel {
					case depLogError:
						r.logger.Error("dependency assertion failed: an expected dependency was not observed during this test's window",
							zap.String("testcase", testCase.Name),
							zap.String("testset", testSetID),
							zap.Strings("missingDependencies", missingDepNames))
					case depLogDebug:
						// Per-test Debug only: an expected-but-unobserved mock
						// is the routine condition the OBSOLETE demotion exists
						// to tolerate, so a Warn here would add hundreds of new
						// lines per run to any suite with a drifting mock pool.
						// The test-set summary below carries the signal instead.
						r.logger.Debug("expected dependencies were not observed during this test's window (reported only; set test.assertDependencies / --assert-dependencies to fail on this)",
							zap.String("testcase", testCase.Name),
							zap.String("testset", testSetID),
							zap.Strings("missingDependencies", missingDepNames))
					case depLogNone:
					}

					// TestResult.MockMismatches: populated for tests going
					// through this (non-streaming) replay path — regardless of
					// pass/fail or obsolescence. Mirrors the sandbox integration
					// runner's behaviour so report UIs can render expected vs
					// actual mocks for passing tests too, not just the
					// obsolete-mismatch path. The deferred streaming-test path
					// (Phase 2 stream replay) does NOT populate this field
					// today — consumers should treat absence as "data not
					// available for this run mode", not "no mocks". Skip when
					// both sides are empty so the json/yaml field stays absent
					// via omitempty. Also skip when perTestConsumed is unknown
					// (non-instrument GetConsumedMocks failed) so we don't
					// emit ActualMocks built from stale loop-scoped data.
					if perTestConsumedKnown && (len(expectedMockInfos) > 0 || len(actualMockInfos) > 0) {
						testCaseResult.MockMismatches = &models.MockMismatchInfo{
							ExpectedMocks: expectedMockInfos,
							ActualMocks:   actualMockInfos,
						}
					}

					// Existing FailureInfo.MockMismatch semantics preserved:
					// only set when the test became OBSOLETE due to a real
					// mock-pool divergence. Downstream code that branches on
					// "is this an obsolete-mismatch?" keeps working.
					if mockSetMismatch && testStatus == models.TestStatusObsolete {
						testCaseResult.FailureInfo.MockMismatch = &models.MockMismatchInfo{
							ExpectedMocks: expectedMockInfos,
							ActualMocks:   actualMockInfos,
						}
					}
					// A wrong answer demoted to OBSOLETE has no verdict
					// (FailureInfo.MappingStale), and its set has none.
					if testStatus == models.TestStatusObsolete {
						testCaseResult.FailureInfo.MappingStale = mappingStaleReason
					}
					finalTestCaseResults[testCase.Name] = testCaseResult
				} else {
					utils.LogError(r.logger, nil, "test case result is nil")
					// Finalize the capture window and persist a failed result so a
					// miss during this test attaches here instead of leaking into a
					// later test (or vanishing) when we bail out on this internal error.
					failedResult := r.CreateFailedTestResult(testCase, testSetID, started, errors.New("internal error: test case result is nil"))
					attachMisses(failedResult)
					if insErr := r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, failedResult); insErr != nil {
						utils.LogError(r.logger, insErr, "failed to insert failed test case result for nil test case result")
					}
					// Outcome was already counted at lines ~1850-1864; don't double-count here.
					break
				}
			} else {
				utils.LogError(r.logger, nil, "test result is nil")
				// Matcher returned no result (e.g. a comparison path returning
				// (false, nil)). Same as above: finalize the window + persist a
				// failed result so the miss surfaces and can't carry forward.
				failedResult := r.CreateFailedTestResult(testCase, testSetID, started, errors.New("internal error: comparison returned no result"))
				attachMisses(failedResult)
				if insErr := r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, failedResult); insErr != nil {
					utils.LogError(r.logger, insErr, "failed to insert failed test case result for nil comparison result")
				}
				// Outcome was already counted at lines ~1850-1864; don't double-count here.
				break
			}

			// We need to sleep for a second to avoid mismatching of mocks during keploy testing via test-bench
			if r.config.EnableTesting {
				r.logger.Debug("sleeping for a second to avoid mismatching of mocks during keploy testing via test-bench")
				time.Sleep(time.Second)
			}
		}
		tmplPass.finish()
		// Every test this cycle meant to run must have produced a verdict. Short
		// of that, the loop broke out early — a failed result insert, an abort
		// signal, or the agent refusing the per-test filter params because it
		// was replaced mid-set (#4614) — and the tests it never reached are
		// simply absent from the report.
		if cycleIncomplete(currentSuccess, currentFailures, currentObsolete, currentSkipped, len(testsToRun)) {
			stoppedEarly = true
		}
		failure += currentFailures
		success = currentSuccess
		obsolete += currentObsolete
		// The passing tests run again only after a cycle a test failed in,
		// and not once a loss has tainted the set (proven switch): a test
		// this cycle ran clean, before the loss, would be charged the loss in
		// the next one, and that row would be its verdict.
		if currentFailures == 0 || replay == 2 || r.captureTaintsTheSet() {
			success = currentSuccess
			for k, v := range currentPassingMocks {
				passingTotalConsumedMocks[k] = v
			}
			break
		}

		// Otherwise, set up the passing tests to run again in the next replay cycle
		testsToRun = nextTestsToRun
		r.logger.Info("Retrying passing test cases to validate mock consistency", zap.Int("replay", replay+1), zap.Int("remaining_tests", len(testsToRun)))
	}
	for _, tcResult := range finalTestCaseResults {
		insertStart := time.Now()
		err := r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, tcResult)
		if time.Since(insertStart) > 50*time.Millisecond {
			r.logger.Debug("Slow InsertTestCaseResult", zap.Duration("duration", time.Since(insertStart)))
		}
		if err != nil {
			utils.LogError(r.logger, err, "failed to insert final test case result")
			testSetStatus = models.TestSetStatusInternalErr
		}
	}

	if loopErr != nil {
		runTestSetCtxCancel()
	}

	// ====== Phase 2: Execute deferred streaming tests sequentially ======
	// Only run Phase 2 if Phase 1 completed without fatal errors and there are deferred tests.
	if loopErr == nil && !exitLoop && len(streamingTests) > 0 {
		r.logger.Info("Now executing streaming tests",
			zap.String("testset", testSetID),
			zap.Int("count", len(streamingTests)))

		// The streaming half of the inert-knob warning, sharing Phase 1's
		// latch so a test set still emits at most ONE of these however many
		// streaming tests it deferred.
		//
		// This pass never calls resolveTestOutcome, and with the proven switch
		// off it writes no DepResult (see the NOTE further down), so
		// --assert-dependencies cannot promote anything here. (Under the
		// switch it judges its tests on their test mocks itself.) Without this call a CI run that points the flag at a
		// set of SSE/chunked tests exits 0 for an assertion that never
		// executed — the flag would be silently inert, which is exactly the
		// failure mode the flag exists to remove. deferredStreaming is true
		// unconditionally: reaching this block means len(streamingTests) > 0.
		// noEligibleDeps is false here and must stay false: this pass has no
		// per-test filtered expectation list to compute it from, and the
		// deferral is the accurate thing to say about these test cases.
		// consumedFetchFailed is false for the same reason — this pass fetches
		// no per-test consumed set, so it has no fetch outcome to report.
		r.warnDependencyAssertionInert(testSetID, useMappingBased, isMappingEnabled, false, true, false, &depAssertionInertWarned)

		for i, deferred := range streamingTests {
			tc := deferred.testCase

			// Set isLastTestCase on the last deferred streaming test: it is the true last
			// test to execute in the entire run, so finalization must trigger here, not in Phase 1.
			if i == len(streamingTests)-1 && r.isLastTestSet {
				r.isLastTestCase = true
				tc.IsLast = true
			}

			// Check for exit signals before each streaming test
			select {
			case <-exitLoopChan:
				testSetStatus = testSetStatusByErrChan
				exitLoop = true
			default:
			}
			if status, ended := endedPartWay(); ended {
				testSetStatus, exitLoop = status, true
			}
			if exitLoop {
				break
			}

			// Exact serving: as the main loop names its test case's calls in
			// flight (the pass's first, or after one that did not open).
			if len(refusedTests[tc.Name]) == 0 {
				r.serveExactAhead(runTestSetCtx, r.exact, tc.Name)
			}

			// Run pre-test mutation before computing the mock window so that
			// any timestamp fields decrypted by the mutator feed into the filter.
			// Streaming Phase 2 has no retry loop so the replay==0 guard used
			// in Phase 1 is not needed here — each tc is executed exactly once.
			if mutator, ok := r.hookImpl.(TestCaseMutator); ok {
				if err := mutator.BeforeTestCaseRun(runTestSetCtx, tc, testSetID); err != nil {
					utils.LogError(r.logger, err, "BeforeTestCaseRun hook failed; replay continues with test case in current state",
						zap.String("testcase", tc.Name),
						zap.String("next_step", "check the BeforeTestCaseRun implementation and any external dependencies it uses (e.g. KMS, auth, network)"))
				}
			}

			// The base path goes in a streaming test's URL here: Phase 1, which
			// puts it in every other test's, defers these before that point.
			if err := r.applyBasePath(tc); err != nil {
				failure++
				testSetStatus = models.TestSetStatusFailed
				if loopErr = r.failUnsentTest(runTestSetCtx, testRunID, testSetID, tc, err); loopErr != nil {
					break
				}
				continue
			}
			// A placeholder refused when the set loaded (a streamed response
			// never captures): the test case fails with the reason, unsent.
			if why := refusedTests[tc.Name]; len(why) > 0 {
				failure++
				testSetStatus = models.TestSetStatusFailed
				if loopErr = r.failRefusedTest(runTestSetCtx, testRunID, testSetID, tc, why); loopErr != nil {
					break
				}
				continue
			}

			// Mock Window: Calculate the effective mock filter window for streaming
			// using the request timestamp to the response timestamp plus a timeout buffer.
			streamReqTime, streamRespTime := effectiveStreamMockWindow(tc, r.config.Test.APITimeout)
			// Merge startup names at the SEND site, not where expectedMocks was
			// built: the same slice is reused below for isMockSubsetWithConfig,
			// and only the agent's loader needs the wider list.
			//
			// The hazard of widening it at the build site is a FALSE PASS, not a
			// false mismatch. isMockSubsetWithConfig iterates CONSUMED and flags
			// anything absent from the expected set, so a larger expected set can
			// only ever report fewer mismatches. A per-test-lifetime mock that
			// happened to be consumed at boot lands in `startup:`, and adding it
			// to expected would have that unexpected consumption tolerated — a
			// test that should go OBSOLETE would pass instead.
			streamExpected := models.MergeStartupMockNames(expectedTestMockMappings[tc.Name], startupMockNames)
			// Open the per-test capture window before simulation, and before the
			// agent's mock window moves to this test (see the non-streaming
			// path). Unlike the non-streaming path we must NOT finalize it right
			// after SimulateRequest (which returns at response headers) —
			// outgoing mock calls keep happening while CompareHTTPStream consumes
			// the stream body below. Every exit path therefore calls
			// attachMockErrors only AFTER stream consumption has finished.
			r.openTestErrorCapture(runTestSetCtx, &captureOpened)
			if r.exact != nil {
				err = r.serveExactTest(runTestSetCtx, r.exact, tc.Name, requestVariables(tmplRun, tc)...)
			} else {
				err = r.SendMockFilterParamsToAgent(runTestSetCtx, streamExpected, streamReqTime, streamRespTime, r.consumedForAgent(totalConsumedMocks, perTestRegion), useMappingBased, recordedSetShape{}, requestVariables(tmplRun, tc)...)
			}
			if err != nil {
				utils.LogError(r.logger, err, "failed to update mock parameters for streaming test")
				loopErr = err
				break
			}
			// Exact serving: as the main loop reads what the gap answered.
			answeredInGap, gapReadErr := r.drainAnsweredInGap(runTestSetCtx, r.exact, tc.Name)

			// Execute: SimulateRequest returns once response headers arrive;
			// for streaming cases the body reader is drained later by
			// CompareHTTPStream below, so in-stream mock consumption can
			// continue after this call returns.
			started := time.Now().UTC()
			// Sent with this run's template values, on a copy; what it is
			// expected to stream, likewise (a streamed response never
			// captures).
			sent := toSend(tmplRun, tc)
			resp, simErr := r.hookImpl.SimulateRequest(runTestSetCtx, &sent, testSetID)
			expectedStream := streamView(tmplRun, &sent)
			// What the stream delivered, kept only when a recorded frame
			// holds a use (streamTemplateFailures).
			var received *bytes.Buffer
			// Why the stream was not read to its end, when the app kept it
			// open on keepalives, past its recording or short of it
			// (pkg.ErrStreamHeldOpen, row W1).
			var streamHeldOpen string

			// Mirror the non-streaming reset-resend: a docker userland-proxy reset on
			// a freshly-accepted host-port conn under CI load never reached the app
			// (zero mocks consumed), so re-send rather than synthesize a false got=0
			// failure. retryResetOnce returns a fresh streaming response for a
			// streaming tc; on the unsafe refusal path we fold its drained mocks into
			// totalConsumedMocks identically to the non-streaming loop.
			if simErr != nil && pkg.IsTransportConnReset(simErr) {
				retryResp, retried, drainedConsumed := r.retryResetOnce(runTestSetCtx, &sent, testSetID, simErr)
				if retried {
					resp, simErr = retryResp, nil
				} else {
					for _, m := range drainedConsumed {
						totalConsumedMocks[m.Name] = m
					}
				}
			}

			if simErr != nil {
				if status, ended := endedPartWay(); ended {
					testSetStatus, exitLoop = status, true
					break
				}
			}

			if simErr != nil {
				sayNoAnswer(r.logger, tc.Name, simErr, "failed to simulate streaming request")
				noteNoAnswer(tc.Name, simErr)
				drainBeforeFiling(runTestSetCtx, tc.Name, answeredInGap, gapReadErr)
				failure++
				testSetStatus = models.TestSetStatusFailed
				testCaseResult := r.CreateFailedTestResult(expectedStream, testSetID, started, simErr)
				markAnswerStalled(testCaseResult, simErr)
				r.attachMockErrors(runTestSetCtx, testSetID, tc.Name, testCaseResult)
				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if loopErr != nil {
					utils.LogError(r.logger, loopErr, "failed to insert streaming test case result for simulation error")
					break
				}
				continue
			}

			httpResp, ok := resp.(*models.HTTPResp)
			var hadStreamingMismatch bool              // Track if streaming comparison failed
			var streamMismatch *pkg.StreamMismatchInfo // Detailed mismatch info for body result

			// Pre-stream drain + mismatch snapshot: needed to decide whether
			// CompareHTTPStream below should emit failure logs. The mismatch
			// is re-evaluated after the post-stream drain so extra mocks
			// consumed while reading the stream body are counted too.
			mockNames := make([]string, 0)
			// streamConsumedErr is the first failure to read what this test
			// consumed, before or after its stream was read.
			var streamConsumedErr error
			if r.instrument {
				consumedMocks, err = r.hookImpl.GetConsumedMocks(runTestSetCtx)
				if err == nil && gapReadErr != nil {
					err = gapReadErr
				}
				if err == nil && len(answeredInGap) > 0 {
					consumedMocks = append(answeredInGap, consumedMocks...)
				}
				if err != nil {
					streamConsumedErr = err
					utils.LogError(r.logger, err, "failed to get consumed filtered mocks for streaming test")
				} else {
					for _, m := range consumedMocks {
						totalConsumedMocks[m.Name] = m
						mockNames = append(mockNames, m.Name)
					}
					noteConsumers(consumers, tc.Name, consumedMocks)
				}
			}

			expectedMocks := deferred.expectedMocks
			mockSetMismatch := false
			hasExpectedMocks := len(expectedMocks) > 0
			if r.instrument && useMappingBased && isMappingEnabled && hasExpectedMocks {
				mockSetMismatch = !isMockSubsetWithConfig(streamAssertedConsumption(carryAttr.withoutStartupTraffic(consumedMocks, startupMockNames, tc.Name), reusableMockNames), expectedMocks)
			}
			// NOTE: with the proven switch off models.Result.DepResult is
			// deliberately NOT populated on the deferred streaming path, for
			// the same reason MockMismatches is not (see
			// models.TestResult.MockMismatches). deferred.expectedMocks is an
			// un-tier-filtered name slice built before the run, so
			// reusable/startup-tier mocks would show up as per-test
			// dependencies and go "missing" at random. Consumers must treat an
			// absent DepResult as "not available for this run mode", never as
			// "this test had no dependencies". Under the switch the rows are
			// written (attachStreamDepResults), from the filtered list the
			// streamed test's own verdict reads.
			//
			// Under the switch, or --strict-failure, a diverged mock set
			// demotes nothing, so the diff of a failed answer is printed as
			// the main loop prints it.
			// Every failed answer's diff is printed, as on the regular path.
			emitFailureLogs := true

			if !ok {
				// Handle streaming response type
				streamResp, streamOk := resp.(*pkg.StreamingHTTPResponse)
				if streamOk {
					noiseConfig := r.config.Test.GlobalNoise.Global
					if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
						noiseConfig = LeftJoinNoise(r.config.Test.GlobalNoise.Global, tsNoise)
					}
					streamBodyNoise := map[string][]string{}
					if bodyNoise, ok := noiseConfig["body"]; ok {
						streamBodyNoise = matcherUtils.CloneNoiseMap(bodyNoise)
					}
					jsonNoiseKeys := pkg.CollectStreamingGlobalNoiseKeys(streamBodyNoise, tc.Noise)

					// When a recorded frame holds a use, the bytes the
					// comparison reads are kept too: each use is asserted
					// against the frame received at its position, whatever
					// the comparison's noise says.
					var streamReader io.Reader = streamResp.Reader
					if tmplRun.StreamHasUses(sent.HTTPResp) {
						received = &bytes.Buffer{}
						streamReader = io.TeeReader(streamResp.Reader, received)
					}
					streamMatched, capturedBody, streamMismatchInfo, streamErr := pkg.CompareHTTPStream(expectedStream.HTTPResp, streamReader, streamResp.StreamConfig, jsonNoiseKeys, r.logger)
					if closeErr := streamResp.Reader.Close(); closeErr != nil {
						r.logger.Debug("failed to close streaming response reader", zap.Error(closeErr))
					}
					if errors.Is(streamErr, pkg.ErrStreamHeldOpen) {
						// The app kept the stream open on keepalives, past
						// its recording or short of it: compared as far as
						// it came, and the test has no verdict (row W1).
						streamHeldOpen, streamErr = streamErr.Error(), nil
					}
					if pkg.AnswerBegan(streamErr) {
						// Its status and headers came, and nothing of the
						// stream: judged on them first, apart from the
						// template and capture path. A start that differs is
						// a judged failure; only one that matches fails
						// closed, not judged, below.
						start := &models.HTTPResp{StatusCode: streamResp.StatusCode, StatusMessage: streamResp.StatusMessage, Header: streamResp.Header}
						if testCaseResult, differs := r.startFailedRow(tmplRun, &sent, expectedStream, start, testSetID, started); differs {
							sayStartJudged(r.logger, tc.Name, streamErr)
							noteNoAnswer(tc.Name, streamErr)
							drainBeforeFiling(runTestSetCtx, tc.Name, answeredInGap, gapReadErr)
							failure++
							testSetStatus = models.TestSetStatusFailed
							r.attachMockErrors(runTestSetCtx, testSetID, tc.Name, testCaseResult)
							loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
							if loopErr != nil {
								utils.LogError(r.logger, loopErr, "failed to save a streaming test result judged on its start")
								break
							}
							continue
						}
					}

					if streamErr != nil {
						if status, ended := endedPartWay(); ended {
							testSetStatus, exitLoop = status, true
							break
						}
					}

					if streamErr != nil {
						if errors.Is(streamErr, pkg.ErrAnswerStalled) {
							// Nothing of the stream came, the app keeping
							// it open (the stall rule, or its keepalives):
							// not judged, failing closed (ruling a).
							sayNoAnswer(r.logger, tc.Name, streamErr, "failed to read streaming response")
						} else {
							r.logger.Error("failed to read streaming response", zap.Error(streamErr))
						}
						noteNoAnswer(tc.Name, streamErr)
						drainBeforeFiling(runTestSetCtx, tc.Name, answeredInGap, gapReadErr)
						failure++
						testSetStatus = models.TestSetStatusFailed
						testCaseResult := r.CreateFailedTestResult(expectedStream, testSetID, started, streamErr)
						markAnswerStalled(testCaseResult, streamErr)
						r.attachMockErrors(runTestSetCtx, testSetID, tc.Name, testCaseResult)
						loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
						if loopErr != nil {
							utils.LogError(r.logger, loopErr, "failed to save streaming test result")
							break
						}
						continue
					}

					if !streamMatched {
						r.logger.Error("streaming response mismatch detected", zap.String("testcase", tc.Name))
						hadStreamingMismatch = true
						streamMismatch = streamMismatchInfo
						// Suppress HTTP matcher logs since we detected streaming mismatch
						emitFailureLogs = false
						// Create HTTPResp with captured body for proper diff display
						httpResp = &models.HTTPResp{
							StatusCode:    streamResp.StatusCode,
							StatusMessage: streamResp.StatusMessage,
							Body:          capturedBody,
							Header:        streamResp.Header,
						}
						// Will be compared by CompareHTTPResp below
					} else {
						bodyForMatcher := capturedBody
						if streamMatched {
							bodyForMatcher = expectedStream.HTTPResp.Body
						}

						httpResp = &models.HTTPResp{
							StatusCode:    streamResp.StatusCode,
							StatusMessage: streamResp.StatusMessage,
							Body:          bodyForMatcher,
							Header:        streamResp.Header,
						}
					}
				} else {
					errMsg := fmt.Sprintf("invalid response type for streaming HTTP test case, got %T", resp)
					r.logger.Error(errMsg)
					failure++
					testSetStatus = models.TestSetStatusFailed
					testCaseResult := r.CreateFailedTestResult(expectedStream, testSetID, started, errors.New(errMsg))
					r.attachMockErrors(runTestSetCtx, testSetID, tc.Name, testCaseResult)
					loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
					if loopErr != nil {
						utils.LogError(r.logger, loopErr, "failed to insert streaming test case result for type assertion error")
						break
					}
					continue
				}
			}

			// The streamed test case's answer is read whole: it ends here.
			// Under exact serving the agent serves the pool from now, so the
			// drain below reads all of the test case's consumption, and a
			// call the application makes from here is the pool's.
			rest := make([]*models.TestCase, 0, len(streamingTests)-i-1)
			for _, s := range streamingTests[i+1:] {
				rest = append(rest, s.testCase)
			}
			if err := r.serveExactPool(runTestSetCtx, r.exact, nextExactTest(rest, nil, nil, refusedTests)); err != nil && streamConsumedErr == nil {
				streamConsumedErr = err
			}

			// Drain mocks consumed during stream body transmission and union
			// with the pre-stream mocks captured earlier. GetConsumedMocks
			// drains on read, so if any mocks land between the pre-stream
			// drain and here (e.g., one backend call per SSE frame, consumed
			// while CompareHTTPStream was still reading frames above), they
			// would otherwise be dropped from mappings.yaml.
			if r.instrument {
				additionalMocks, drainErr := r.hookImpl.GetConsumedMocks(runTestSetCtx)
				if drainErr != nil {
					if streamConsumedErr == nil {
						streamConsumedErr = drainErr
					}
					utils.LogError(r.logger, drainErr, "failed to get consumed filtered mocks for streaming test")
				}
				for _, m := range additionalMocks {
					totalConsumedMocks[m.Name] = m
					mockNames = append(mockNames, m.Name)
				}
				noteConsumers(consumers, tc.Name, additionalMocks)
				consumedMocks = append(consumedMocks, additionalMocks...)
			}

			// Re-evaluate mismatch now that consumedMocks covers the full
			// test case (pre-stream + in-stream). If CompareHTTPStream
			// already forced emitFailureLogs=false on a streaming body
			// mismatch, it stays suppressed: that mismatch was logged.
			if r.instrument && useMappingBased && isMappingEnabled && hasExpectedMocks {
				mockSetMismatch = !isMockSubsetWithConfig(streamAssertedConsumption(carryAttr.withoutStartupTraffic(consumedMocks, startupMockNames, tc.Name), reusableMockNames), expectedMocks)
			}

			// Each use place is asserted on its own: the headers', and each
			// frame's against the frame received at its position, whatever
			// the stream comparison and its noise say. It is read first, so
			// a test case it fails never prints the comparison's pass line.
			var streamReceived string
			// Whether streamReceived is all the answer delivered: a whole
			// answer is, and so is a stream every recorded frame of which
			// matched. A stream that stopped matching was read no further
			// (the test case fails on that), so its later frames are not
			// asserted.
			var streamComplete bool
			switch {
			case received != nil:
				streamReceived, streamComplete = received.String(), !hadStreamingMismatch
			case ok:
				// The app answered whole, not as a stream: its body is what
				// was delivered, frame by frame.
				streamReceived, streamComplete = httpResp.Body, true
			}
			// Under the proven switch a streaming test owns its test mocks too,
			// and fails when one of them was not consumed. Decided once what
			// it consumed is read whole, before the comparison, so that
			// nothing says it passed.
			streamArmed := r.instrument && useMappingBased && isMappingEnabled
			streamTestMocksUnread := false
			streamOwned, streamUnconsumed := unconsumedTestMocks(streamArmed, expectedTestMockMappings[tc.Name], consumedMocks, mockKindByName, reusableMockNames)
			if streamConsumedErr != nil {
				consumptionUnread = appendOnce(consumptionUnread, tc.Name)
			}
			if testMocksUnknown(streamArmed, streamOwned, streamConsumedErr) {
				// Not judged rather than failed on a partial read.
				consumedUnread = appendOnce(consumedUnread, tc.Name)
				streamUnconsumed = nil
				streamTestMocksUnread = true
			}
			// Said here unless the mock-set mismatch line below says it (once).
			if len(streamUnconsumed) > 0 && !mockSetMismatch {
				r.logger.Error("recorded calls of this streaming test were not made during it; failing it (KEPLOY_PROVEN=1)",
					zap.String("testcase", tc.Name),
					zap.String("testset", testSetID),
					zap.Strings("unconsumedMocks", namesOf(streamUnconsumed)))
			}
			// Its misses too, read once, now, and attached below. Under the
			// proven switch a miss fails the test (streamMissed); with it off a
			// miss keeps a test whose mock set diverged from being demoted to
			// OBSOLETE (streamCallsMissed), as in the request-response path.
			streamRead := r.readMockErrors(runTestSetCtx)
			streamCallsMissed := dataPlaneMisses(streamRead.calls)
			streamMissed := 0
			if provenswitch.On() {
				if streamMissed = streamCallsMissed; streamMissed > 0 && !mockSetMismatch {
					r.logger.Error("calls this streaming test made got no recorded answer; failing it (KEPLOY_PROVEN=1); the report lists each under unmatched_calls",
						zap.String("testcase", tc.Name),
						zap.String("testset", testSetID),
						zap.Int("unmatchedCalls", streamMissed))
				}
			}
			// And what keploy's agent could not capture while it ran (or a
			// test before it did): under the proven switch that fails it too.
			// Said here unless the mock-set mismatch line below says it (once).
			streamLostCapture := streamRead.failsOnCaptureLoss()
			if streamLostCapture && !mockSetMismatch {
				r.logger.Error("keploy's agent could not capture every call while this streaming test or a test before it ran; failing it (KEPLOY_PROVEN=1); the report says what under capture_incomplete",
					zap.String("testcase", tc.Name),
					zap.String("testset", testSetID))
			}
			// Every result written for this test below attaches that read.
			attachStreamMisses := func(res *models.TestResult) {
				r.attachReadMockErrors(streamRead, testSetID, tc.Name, res)
				if provenswitch.On() {
					missCategory(res)
				}
			}
			streamTemplateFailures := reportFailures(r.streamTemplateFailures(tmplRun, &tmplCheck, &sent, streamReceived, streamComplete, httpResp, testSetID))
			var streamOpts []httpMatcher.MatchOption
			if len(streamTemplateFailures) > 0 || len(streamUnconsumed) > 0 || streamMissed > 0 || streamLostCapture {
				streamOpts = append(streamOpts, httpMatcher.WithoutPassLine())
			}
			testPass, testResult := r.compareHTTPResp(expectedStream, httpResp, testSetID, emitFailureLogs, streamOpts...)
			if len(streamUnconsumed) > 0 || streamMissed > 0 || streamLostCapture {
				testPass = false
			}
			if testResult == nil {
				// Matcher returned no result (e.g. an internal compare path returning
				// (false, nil)). Handle it HERE, before the hadStreamingMismatch block
				// below dereferences testResult.BodyResult. Finalize the window + persist
				// a failed result so the miss surfaces and can't carry forward.
				utils.LogError(r.logger, nil, "streaming test result is nil")
				failedResult := r.CreateFailedTestResult(expectedStream, testSetID, started, errors.New("internal error: streaming comparison returned no result"))
				attachStreamMisses(failedResult)
				failure++
				testSetStatus = models.TestSetStatusFailed
				if insErr := r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, failedResult); insErr != nil {
					utils.LogError(r.logger, insErr, "failed to insert failed streaming test case result for nil result")
				}
				continue
			}
			if len(streamTemplateFailures) > 0 {
				testPass = false
				markTemplateFailures(testResult)
			}
			// Override testPass if streaming comparison failed
			// (HTTP matcher skips body comparison for non-JSON bodies by default)
			if hadStreamingMismatch {
				testPass = false
				// Add body result showing the mismatched frame diff
				if streamMismatch != nil {
					expectedFrameDisplay := fmt.Sprintf("Frame %d: %s", streamMismatch.FrameIndex, streamMismatch.ExpectedFrame)
					actualFrameDisplay := fmt.Sprintf("Frame %d: %s", streamMismatch.FrameIndex, streamMismatch.ActualFrame)
					if streamMismatch.Reason != "" {
						actualFrameDisplay = fmt.Sprintf("Frame %d (%s): %s", streamMismatch.FrameIndex, streamMismatch.Reason, streamMismatch.ActualFrame)
					}
					testResult.BodyResult = append(testResult.BodyResult, models.BodyResult{
						Normal:   false,
						Type:     models.Plain,
						Expected: expectedFrameDisplay,
						Actual:   actualFrameDisplay,
					})
					// Display the streaming frame diff in same format as HTTP matcher
					logDiffs := matcherUtils.NewDiffsPrinter(tc.Name)
					logDiffs.PushBodyDiff(expectedFrameDisplay, actualFrameDisplay, nil)
					_ = logDiffs.Render()
				}
				// Log failure message in same format as HTTP matcher
				r.logger.Info("result", zap.String("testcase id", models.HighlightFailingString(tc.Name)), zap.String("testset id", models.HighlightFailingString(testSetID)), zap.String("passed", models.HighlightFailingString(false)))
				// Print user-facing failure message with red color on test case ID only (matching HTTP matcher style)
				fmt.Printf("\nTestrun failed for testcase with id: \"%s%s%s\"\n\n--------------------------------------------------------------------\n\n",
					"\033[91m", tc.Name, "\033[0m")
			}

			tcReqTimeStream, tcRespTimeStream := recordReqResTimestamps(tc)
			carryAttr.mapConsumed(actualTestMockMappings, tc.Name, tcReqTimeStream, tcRespTimeStream, consumedMocks)

			// Log consumed mocks for streaming test
			r.logger.Debug("consumed mocks for streaming test case",
				zap.String("testSetID", testSetID),
				zap.String("testCaseID", tc.Name),
				zap.Strings("mockNames", mockNames))

			if mockSetMismatch {
				if testPass {
					r.logger.Debug("mock mapping mismatch ignored because streaming testcase passed",
						zap.String("testcase", tc.Name),
						zap.String("testset", testSetID),
						zap.Strings("expectedMocks", expectedMocks),
						zap.Strings("actualMocks", mockNames))
				} else if len(streamTemplateFailures) > 0 {
					r.logger.Error("mock mapping mismatch detected for streaming testcase; marking it as FAILED",
						zap.String("testcase", tc.Name),
						zap.String("testset", testSetID),
						zap.String("reason", vetoTemplateFailure),
						zap.Strings("expectedMocks", expectedMocks),
						zap.Strings("actualMocks", mockNames))
					r.mockMismatchFailures.AddFailure(testSetID, tc.Name, expectedMocks, mockNames)
				} else if len(streamUnconsumed) > 0 || streamCallsMissed > 0 || streamLostCapture {
					reason := vetoUnconsumedTestMock
					switch {
					case len(streamUnconsumed) > 0:
					case streamCallsMissed > 0 && provenswitch.On():
						reason = vetoUnmatchedCall
					case streamCallsMissed > 0:
						reason = vetoUnmatchedCallSwitchOff
					default:
						reason = vetoCaptureLoss
					}
					r.logger.Error("mock mapping mismatch detected for streaming testcase; marking it as FAILED",
						zap.String("testcase", tc.Name),
						zap.String("testset", testSetID),
						zap.String("reason", reason),
						zap.Strings("expectedMocks", expectedMocks),
						zap.Strings("actualMocks", mockNames))
					r.mockMismatchFailures.AddFailure(testSetID, tc.Name, expectedMocks, mockNames)
				} else if r.strictFailure() {
					// Under the switch, or --strict-failure, a failed answer is
					// FAILED whatever the mock set did (the verdict below): the
					// set's other consumption (a session-tier mock it shares) is
					// no reason to re-record it.
					r.logger.Error("mock mapping mismatch detected for streaming testcase; marking it as FAILED",
						zap.String("testcase", tc.Name),
						zap.String("testset", testSetID),
						zap.String("reason", vetoFlagName(false, true)),
						zap.Strings("expectedMocks", expectedMocks),
						zap.Strings("actualMocks", mockNames))
					r.mockMismatchFailures.AddFailure(testSetID, tc.Name, expectedMocks, mockNames)
				} else {
					r.logger.Error("mock mapping mismatch detected for streaming testcase; marking as obsolete",
						zap.String("testcase", tc.Name),
						zap.String("testset", testSetID),
						zap.Strings("expectedMocks", expectedMocks),
						zap.Strings("actualMocks", mockNames))
					r.mockMismatchFailures.AddFailure(testSetID, tc.Name, expectedMocks, mockNames)
				}
			}

			if !hadStreamingMismatch {
				if !testPass {
					r.logger.Info("result", zap.String("testcase id", models.HighlightFailingString(tc.Name)), zap.String("testset id", models.HighlightFailingString(testSetID)), zap.String("passed", models.HighlightFailingString(testPass)))
				} else {
					r.logger.Info("result", zap.String("testcase id", models.HighlightPassingString(tc.Name)), zap.String("testset id", models.HighlightPassingString(testSetID)), zap.String("passed", models.HighlightPassingString(testPass)))
				}
			}

			//  Record: Update counters and persist result.
			//
			// The OBSOLETE shape for the mapping write: a wrong answer with a
			// recorded mock of its mapping left unconsumed (what mockSetMismatch
			// is in the main loop) or a mock outside it consumed (what it is
			// here).
			streamLeftRecorded := streamArmed && streamConsumedErr == nil &&
				leftRecordedMockUnconsumed(expectedTestMockMappings[tc.Name], consumedMocks, mockKindByName, reusableMockNames)
			if streamLeftRecorded {
				leftRecordedUnconsumed = appendOnce(leftRecordedUnconsumed, tc.Name)
			}
			if !testPass && (mockSetMismatch || streamLeftRecorded) {
				obsoleteShaped = appendOnce(obsoleteShaped, tc.Name)
			}
			var testStatus models.TestStatus
			if testPass {
				testStatus = models.TestStatusPassed
				success++
				for _, m := range consumedMocks {
					passingTotalConsumedMocks[m.Name] = m
				}
			} else if mockSetMismatch && !r.strictFailure() && len(streamTemplateFailures) == 0 && len(streamUnconsumed) == 0 && streamCallsMissed == 0 {
				// A template failure is never demoted to OBSOLETE, nor is a
				// test that left one of its test mocks unconsumed, or one a
				// call of which got no recorded answer, nor, under the proven
				// switch, any test.
				testStatus = models.TestStatusObsolete
				obsolete++
			} else {
				testStatus = models.TestStatusFailed
				failure++
				testSetStatus = models.TestSetStatusFailed
			}
			carryAttr.keepWhateverTheVerdict(passingTotalConsumedMocks, tc.Name, consumedMocks)

			if testResult != nil {
				testCaseResult := &models.TestResult{
					Kind:       models.HTTP,
					Name:       testSetID,
					Status:     testStatus,
					Started:    started.Unix(),
					Completed:  time.Now().UTC().Unix(),
					TestCaseID: tc.Name,
					// The request as it was sent.
					Req: models.HTTPReq{
						Method:     sent.HTTPReq.Method,
						ProtoMajor: sent.HTTPReq.ProtoMajor,
						ProtoMinor: sent.HTTPReq.ProtoMinor,
						URL:        sent.HTTPReq.URL,
						URLParams:  sent.HTTPReq.URLParams,
						Header:     sent.HTTPReq.Header,
						Body:       sent.HTTPReq.Body,
						Binary:     sent.HTTPReq.Binary,
						Form:       sent.HTTPReq.Form,
						Timestamp:  sent.HTTPReq.Timestamp,

						HeaderLineLengths: sent.HTTPReq.HeaderLineLengths,
					},
					Res:          *httpResp,
					TestCasePath: filepath.Join(r.config.Path, testSetID),
					MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
					Noise:        tc.Noise,
					Result:       *testResult,
					TimeTaken:    time.Since(started).String(),

					TemplateFailures: streamTemplateFailures,
				}

				if testStatus == models.TestStatusFailed && testResult.FailureInfo.Risk != models.None {
					testCaseResult.FailureInfo.Risk = testResult.FailureInfo.Risk
					testCaseResult.FailureInfo.Category = testResult.FailureInfo.Category
				}
				// Under the proven switch a streamed test's test mocks are
				// asserted as the main loop's are (unconsumedTestMocks), so its
				// report carries the same rows and the same evidence: the
				// presence rows (Result.DepResult), its DEPENDENCY_MISSING
				// category, and the unconsumed test mocks by name. The inputs
				// are the ones that verdict read.
				testCaseResult.FailureInfo.TestMocksUnread = streamTestMocksUnread
				testCaseResult.FailureInfo.StreamHeldOpen = streamHeldOpen
				if testStatus == models.TestStatusObsolete {
					testCaseResult.FailureInfo.MappingStale = mappingStaleReason
				}
				if provenswitch.On() {
					attachStreamDepResults(testCaseResult, testStatus, expectedTestMockMappings[tc.Name], consumedMocks, streamArmed && streamConsumedErr == nil, reusableMockNames, mockKindByName, mockLookup)
				}

				// Finalize the capture window now - AFTER CompareHTTPStream and the
				// post-stream consumed-mock drain above, so in-stream mock misses
				// are included - and attach them to this result.
				attachStreamMisses(testCaseResult)

				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if loopErr != nil {
					utils.LogError(r.logger, loopErr, "failed to save streaming test result")
					break
				}
			} else {
				// Unreachable: a nil testResult is finalized right after CompareHTTPResp
				// above (persists a failed result and continues). Defensive only.
				utils.LogError(r.logger, nil, "streaming test result is nil")
				break
			}
		}
	}

	// Prune and mappings only; the report and stoppedEarly are unchanged. Every
	// loaded, non-ignored test must have a verdict from Phase 1 or Phase 2 (each
	// path counts one before it continues or breaks). stoppedEarly cannot say
	// this: it counts against testsToRun, which the selection already narrowed
	// (a selected-test list, a selection loop cut short) and which leaves out
	// the streaming tests.
	toScore := 0
	for _, tc := range testCases {
		if _, ok := ignoredTests[tc.Name]; !ok {
			toScore++
		}
	}
	allTestsGotAVerdict := success+failure+obsolete == toScore

	// Fold every mock consumed since the last rewind into the cross-cycle
	// union — the final main-loop cycle's mocks and the streaming Phase 2
	// mocks (Phase 2 appends to totalConsumedMocks and never rewinds).
	// Earlier retry cycles were already folded at each rewind above. This
	// must run after Phase 2's last write so the post-loop readers
	// (PersistMockNoise and the Mocks-Consumed telemetry) see the complete
	// union of everything consumed across all cycles and phases.
	for k, v := range totalConsumedMocks {
		allConsumedAcrossCycles[k] = v
	}

	// ONE summary line per test set for dependencies that were recorded but
	// not observed, in the default (knob off) mode. Per-test this is logged
	// at Debug: it is the routine condition the OBSOLETE demotion tolerates,
	// and a Warn per test would add hundreds of lines per run to any suite
	// with a drifting mock pool — a CLI-output regression for users who never
	// opted into the assertion. The count and a sample still surface here.
	r.warnUnexercisedDependencies(testSetID, depMissingTests)

	timeTaken := time.Since(startTime)

	testCaseResults, resultsErr := r.reportDB.GetTestCaseResults(runTestSetCtx, testRunID, testSetID)
	if resultsErr != nil {
		if runTestSetCtx.Err() != context.Canceled {
			if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), resultsErr); ok {
				testSetStatus = resolvedStatus
			} else {
				utils.LogError(r.logger, resultsErr, "failed to get test case results")
				testSetStatus = models.TestSetStatusInternalErr
			}
		}
	}
	// The prune's connection-error rule reads these back: unreadable or short of
	// the verdicts, they cannot vouch for anything (fail closed). They are
	// counted by test, as the verdicts count them (verdictCounts): a test
	// with two rows (an earlier cycle's and its last) is one test, so its
	// second row cannot stand in for another test's lost one.
	scoredTests := make(map[string]struct{}, len(testCaseResults))
	for _, tr := range testCaseResults {
		if tr.Status != models.TestStatusIgnored {
			scoredTests[tr.TestCaseID] = struct{}{}
		}
	}
	resultsComplete := resultsErr == nil && len(scoredTests) >= success+failure+obsolete

	// Under the proven switch an unconsumed test mock a sibling consumed is
	// named with that sibling.
	if pairs := markConsumedBy(testCaseResults, consumers); len(pairs) > 0 {
		r.logger.Warn("tests failed for a recorded call that a sibling test made instead: the order the shared mock pool served them in can decide this until each test's consumption is exact (KEPLOY_PROVEN=1); each is named under unconsumed_mocks.consumed_by",
			zap.String("test-set", testSetID),
			zap.Strings("tests", pairs))
	}
	// Under the proven switch a call no mock answered whose closest
	// recording another test owns is named with that test: likely a call it
	// recorded, such as a later exchange of a connection it opened (a
	// Generic connection's handshake is recorded in the test that opened it).
	if named := markRecordedIn(testCaseResults, r.exact.ownersOf()); len(named) > 0 {
		r.logger.Warn("tests made a call that another test recorded: likely, at record time that test made it on a connection it opened, whose handshake was recorded there (KEPLOY_PROVEN=1); each is named under unmatched_calls.recorded_in",
			zap.String("test-set", testSetID),
			zap.Strings("calls", named[:min(len(named), 20)]),
			zap.String("next_step", "record the test set again so each test opens the connections it uses, or keep the application's connection reuse as it was when recording"))
	}
	// Under the proven switch a test whose call got no recorded answer fails,
	// before the hook, so the hook sees the verdicts it is to act on. The loop
	// failed each already (earlyMissed, streamMissed); this finds one only if a
	// path wrote a result without that verdict.
	if missed := failTestsWithMisses(testCaseResults); len(missed) > 0 {
		r.logger.Error("tests failed: a call they made got no recorded answer (KEPLOY_PROVEN=1); the report lists each call under unmatched_calls",
			zap.String("test-set", testSetID),
			zap.Strings("tests", missed))
	}
	// Read before any hook runs, as the evidence floor reads it: a loss while
	// a test ran holds the prune and mappings.yaml below. The loop failed
	// each test it charged a loss already (lostCapture, streamLostCapture);
	// failTestsWithCaptureLoss finds one only if a path wrote a result
	// without that verdict, and adds CAPTURE_INCOMPLETE to each.
	testsLostCapture := anyTestLostCapture(testCaseResults)
	if tainted := failTestsWithCaptureLoss(testCaseResults); len(tainted) > 0 {
		r.logger.Error("tests failed: keploy's agent could not capture every call while they or a test before them ran (KEPLOY_PROVEN=1); the report says what under capture_incomplete",
			zap.String("test-set", testSetID),
			zap.Strings("tests", tainted))
	}
	// The evidence against each test, as the loop wrote it, before any hook can
	// touch it (holdEvidenceFloor reads it back).
	evidence := snapshotEvidence(testCaseResults)
	err = r.hookImpl.BeforeTestResult(ctx, testRunID, testSetID, testCaseResults)
	if err != nil {
		stopReason := fmt.Sprintf("failed to run before test result hook: %v", err)
		utils.LogError(r.logger, err, stopReason)
	}
	// Under the proven switch the per-test verdicts, as the hook left them,
	// drive the set: its counters, its status, and so the report, JUnit and the
	// exit code. A hook may quarantine a test (IGNORED), but a test with
	// evidence against it (a call no mock answered, a recorded call it did not
	// make, as the loop wrote it before the hook ran) stays FAILED whatever the
	// hook set, removed or cleared: that is held here, once, for every hook,
	// test by test. A status that is no verdict counts as failed. The verdicts
	// drive the set only when every test's result row is there to count;
	// otherwise the loop's status stands. A set whose tests' consumption could
	// not all be read has no verdict at all.
	notJudged := ""
	// appLogsBeforeStop is what the app logged last, read before the set's
	// end stopped it (stopAppBeforeSetEnd), for a report that carries logs.
	appLogsBeforeStop := ""
	// consumedAtStopUnread is why what the app consumed as the set's end
	// stopped it could not be read.
	var consumedAtStopUnread error
	// stopKilled is why the set's end had to kill the app it stopped: its
	// shutdown is then not known, and the set is not judged.
	stopKilled := ""
	// realRun is what the set's run saw of mock.real beyond its test cases;
	// realListed the destinations the recordings of the test cases that ran
	// list; realWhy why they fail the set (proven switch).
	var realRun *models.RealRun
	var realListed []string
	realWhy, realUncaptured := "", ""
	// unclaimed are the calls the application made after the set's last test
	// that no mock answered (proven switch): no later test takes them, so
	// they fail the set (unclaimed_calls.go).
	var unclaimed []models.UnmatchedCall
	unclaimedReason := ""
	// workerMissing: under the proven switch, the set's worker-scope calls
	// its run reached the place of that the application never made
	// (awaitWorkerScope).
	var workerMissing []string
	// appNotStopped is why the set's end was read with its app still running
	// (P101-c), "" when keploy stopped it first.
	appNotStopped := ""
	if provenswitch.On() {
		var held []string
		scored := len(testCaseResults)
		testCaseResults, held = holdEvidenceFloor(testCaseResults, evidence)
		// A test the floor put back is written to the report store too: the
		// run's failed-test list is read back from it.
		for i := scored; i < len(testCaseResults); i++ {
			if insErr := r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, &testCaseResults[i]); insErr != nil {
				utils.LogError(r.logger, insErr, "failed to insert a test result the evidence floor put back")
			}
		}
		if len(held) > 0 {
			r.logger.Error("a before-test-result hook passed, relabelled or removed tests with evidence against them, or cleared that evidence; they stay FAILED (KEPLOY_PROVEN=1)",
				zap.String("test-set", testSetID),
				zap.Strings("tests", held))
		}
		tally := tallyFromVerdicts(setTally{success, failure, obsolete, ignored, testSetStatus}, testCaseResults, resultsComplete)
		success, failure, obsolete, ignored, testSetStatus = tally.success, tally.failure, tally.obsolete, tally.ignored, tally.status
		// The app keploy started for this set is stopped, and waited for,
		// before the set's end reads what it did after its last test (what
		// it reached of mock.real, the calls no mock answered, what keploy
		// could not capture): a call it makes only as it is stopped (a
		// publish its shutdown flushes, a write on its way out) is then read
		// with them, not left for no one to judge (P101-b). The agent is told
		// first, as the teardown tells it, so the app's shutdown errors are
		// logged as such. Its recent logs are taken before: a container
		// docker removes once stopped has none to give.
		// The set's worker scope (D3): before its application is stopped,
		// every worker-scope call its run reached the place of must have been
		// made. A worker makes its calls in its own time, so the end waits
		// for them, as long as the application lives and the calls keep
		// coming; one never made fails the set below.
		if r.exact != nil && captureOpened {
			workerMissing = r.awaitWorkerScope(runTestSetCtx, r.exact, totalConsumedMocks, func(read []models.MockState) {
				for _, m := range read {
					totalConsumedMocks[m.Name] = m
					allConsumedAcrossCycles[m.Name] = m
				}
			})
		}
		// The final attempt of a run's last set stops the app --keep-app-alive
		// keeps across sets before its end's read, as a set stops its own
		// (P101-c): the run ends after it, and no attempt of it follows.
		if stopAppBeforeSetEnd == nil && serveTest && r.isLastTestSet && r.finalAttempt && r.keepAliveStop != nil {
			stopAppBeforeSetEnd = r.keepAliveStop
		}
		if stopAppBeforeSetEnd != nil {
			notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := r.instrumentation.NotifyGracefulShutdown(notifyCtx); err != nil {
				r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
			}
			notifyCancel()
			logCtx, cancelLogs := context.WithTimeout(context.WithoutCancel(runTestSetCtx), 10*time.Second)
			appLogsBeforeStop = r.instrumentation.GetRecentAppLogs(logCtx)
			cancelLogs()
			stopKilled = stopAppBeforeSetEnd()
			// What the app consumed as it stopped (a shutdown call a pool
			// mock answered) is drained now, into the set's: what the learn
			// step learned on it is then kept with the set's (persistLearned),
			// and the next set's first read does not take it, by its name,
			// for one of its own.
			//
			// Not on the set's context: the app watcher cancels it once an
			// app that exited on its own is recorded, and what that app
			// consumed is the set's all the same. On the run's, which the
			// user's stop cancels, and bounded (setEndDrainBudget): the
			// agent's client has no deadline of its own, and a read that does
			// not end leaves the set not judged, never a run that does not
			// end.
			drainCtx, cancelDrain := context.WithTimeout(ctx, setEndDrainBudget)
			consumed, err := r.hookImpl.GetConsumedMocks(drainCtx)
			cancelDrain()
			if err != nil {
				consumedAtStopUnread = err
			} else {
				for _, m := range consumed {
					totalConsumedMocks[m.Name] = m
					allConsumedAcrossCycles[m.Name] = m
				}
			}
		}
		// stoppedByUser records, once the run's user has stopped it (Ctrl+C,
		// SIGTERM), that the set is USER_ABORT, and reports it. The app
		// watcher records the same, but on a goroutine of its own, after the
		// reads below may already have failed for the stop: one the stop cut
		// short is the stop's, never a set whose calls could not be read.
		//
		// The run's context, not the set's: the app watcher cancels the set's
		// once it has recorded how the app ended, and that is no user's stop.
		stoppedByUser := func() bool {
			if !stoppedByItsUser(ctx) {
				return false
			}
			if testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed {
				testSetStatus = models.TestSetStatusUserAbort
			}
			return true
		}
		stoppedByUser()
		// mock.real (design v14 §4, §8.2): what the set's run reached, and
		// what keploy could not capture outside its test cases.
		var realErr error
		if !r.noAgent {
			// A run with no agent (a base path) has no REAL bookkeeping.
			realRun, realErr = realclient.Run(context.WithoutCancel(runTestSetCtx), r.instrumentation, r.mockReal())
		}
		switch {
		case realErr != nil && (testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed):
			notJudged = "keploy could not read the set's mock.real destinations from its agent: " + realErr.Error()
			r.logger.Error("test-set is not judged: keploy could not read which mock.real destinations its run reached; the run goes on to the next test-set",
				zap.String("test-set", testSetID), zap.Error(realErr))
			testSetStatus = models.TestSetStatusInternalErr
			r.markNotJudged(testSetID)
		case realRun != nil:
			realListed = realListedBy(testCases, testCaseResults)
			if h, ok := r.hookImpl.(RealRunHook); ok {
				h.SetRealRun(testSetID, realRun, realListed)
			}
			if why := realSetFailure(realRun, realListed, realUsedBy(testCaseResults)); why != "" {
				realWhy = why
				r.logger.Error("test-set failed: "+why, zap.String("test-set", testSetID))
				if testSetStatus == models.TestSetStatusPassed {
					testSetStatus = models.TestSetStatusFailed
				}
			}
			// What keploy could not capture outside every test case is the
			// set's capture verdict (rule 2), with the set's other losses
			// below: it fails the set, holds the prune, and fails JUnit.
			realUncaptured = models.RealUncapturedLosses(realRun.Incomplete).Text()
		}
		// A call made after the set's last test that no mock answered is a
		// pool miss no later test takes: it fails the set, named. A read that
		// cannot vouch for every such call leaves the set unjudged. Only a
		// set with a verdict to give is read: one that stopped otherwise
		// (the app halted, the user aborted, keploy failed) keeps its status.
		// And only once a test of it opened its window: before that, what
		// the agent holds is the app's startup, which the first test's read
		// hands over as the set's. A call made then that no mock answered
		// fails the set too, named, and no test (before_set_calls.go).
		//
		// A stop that came after the loop (the app exited, the user
		// stopped the run) is not in testSetStatus yet: resolveTestSetStatus
		// below takes it from getErrStatus. Such a set is not read, and a
		// read that fails because the set was stopped meanwhile (its ctx is
		// cancelled) is the stop's, not the read's: the stop is its status,
		// as before, and it is not marked not judged, which would also keep
		// the run going (abortAfterTestSet) past an app that exited.
		_, stoppedBefore := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), nil)
		if captureOpened && !stoppedBefore && (testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed) {
			if stopAppBeforeSetEnd == nil {
				appNotStopped = whyAppNotStopped(cmdType, serveTest)
				r.logger.Warn("keploy reads the set's end with its application still running: a call it makes after this read is not judged ("+provenswitch.Env+"=1)",
					zap.String("test-set", testSetID), zap.String("reason", appNotStopped))
			}
			calls, unclaimedErr := r.unclaimedCalls(runTestSetCtx)
			if len(workerMissing) > 0 {
				unclaimedReason = workerWhy(workerMissing)
				r.logger.Error("test-set failed: "+unclaimedReason+" ("+provenswitch.Env+"=1)",
					zap.String("test-set", testSetID), zap.Strings("mocks", workerMissing),
					zap.String("next_step", "if the application no longer makes these calls on its own, record the test set again"))
				if testSetStatus == models.TestSetStatusPassed {
					testSetStatus = models.TestSetStatusFailed
				}
			}
			if own := r.setCalls.failing(); len(own) > 0 {
				unclaimed = append(unclaimed, own...)
				if unclaimedReason != "" {
					unclaimedReason += "; "
				}
				unclaimedReason += setCallsWhy(own)
				r.logSetCalls(testSetID, own)
				if testSetStatus == models.TestSetStatusPassed {
					testSetStatus = models.TestSetStatusFailed
				}
			}
			if len(calls) > 0 {
				unclaimed = append(unclaimed, calls...)
				if unclaimedReason != "" {
					unclaimedReason += "; "
				}
				unclaimedReason += unclaimedWhy(calls)
				r.logUnclaimed(testSetID, calls)
				switch {
				case conf.RecordStopKilled != "":
					// The recording's stop killed the app: what it did as
					// it stopped is not in the recording, so such a call
					// may be one the recording lacks (P101-b-k).
					notJudged = alsoNotJudged(notJudged, "not judged: "+unclaimedReason+"; the recording's stop had to kill the application ("+conf.RecordStopKilled+"), so what it did as it stopped is not in the recording")
					r.logger.Error("test-set is not judged: its application made calls after the set's last test that no mock answered, and its recording lacks what the application did as it stopped (the recording's stop had to kill it); the run goes on to the next test-set ("+provenswitch.Env+"=1)",
						zap.String("test-set", testSetID), zap.String("recording", conf.RecordStopKilled),
						zap.String("next_step", "make the application exit promptly on SIGINT, then record the test-set again"))
					testSetStatus = models.TestSetStatusInternalErr
					r.markNotJudged(testSetID)
				case testSetStatus == models.TestSetStatusPassed:
					testSetStatus = models.TestSetStatusFailed
				}
			}
			if _, stopped := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), nil); unclaimedErr != nil && runTestSetCtx.Err() != nil && (stopped || stoppedByUser()) {
				r.logger.Debug("the read of the calls made after the set's last test was cut short by the set's stop; the stop is its status",
					zap.String("test-set", testSetID), zap.Error(unclaimedErr))
				unclaimedErr = nil
			}
			if unclaimedErr != nil {
				notJudged = alsoNotJudged(notJudged, "keploy could not read every call the application made after the set's last test that no mock answered: "+unclaimedErr.Error())
				r.logger.Error("test-set is not judged: keploy could not read every call its application made after its last test; the run goes on to the next test-set",
					zap.String("test-set", testSetID), zap.Int("callsRead", len(calls)), zap.Error(unclaimedErr))
				testSetStatus = models.TestSetStatusInternalErr
				r.markNotJudged(testSetID)
			}
		}
		if len(consumedUnread) > 0 && (testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed) {
			notJudged = consumedUnreadReason(consumedUnread)
			r.logger.Error("test-set is not judged: what some of its tests consumed could not be read from keploy's agent; the run goes on to the next test-set",
				zap.String("test-set", testSetID),
				zap.Strings("tests", consumedUnread))
			testSetStatus = models.TestSetStatusInternalErr
			// Not judged, not broken: the round goes on (abortAfterTestSet).
			r.markNotJudged(testSetID)
		}
		// The set's end had to kill the app it stopped (it had not exited
		// when the stop's grace ran out): what the app would have done as
		// it stopped is not known, so neither is the set's verdict.
		if stopKilled != "" && (testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed || notJudged != "") {
			notJudged = alsoNotJudged(notJudged, "not judged: "+stopKilled+", and what it would have done as it stopped is not known")
			r.logger.Error("test-set is not judged: keploy had to kill the application it stopped at the set's end; the run goes on to the next test-set ("+provenswitch.Env+"=1)",
				zap.String("test-set", testSetID), zap.String("reason", stopKilled),
				zap.String("next_step", "make the application exit promptly on SIGINT (a shutdown that waits on something slow or that never comes, a handler that ignores it), then run the test-set again"))
			testSetStatus = models.TestSetStatusInternalErr
			r.markNotJudged(testSetID)
		}
		// What the app consumed as the set's end stopped it could not be
		// read: what was learned on it cannot be vouched for. A set that
		// stopped otherwise (the app halted, the user aborted) keeps that
		// status, as the read of its calls after the last test does.
		if _, stopped := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), nil); consumedAtStopUnread != nil && !stopped && !stoppedByUser() &&
			(testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed || notJudged != "") {
			notJudged = alsoNotJudged(notJudged, "not judged: keploy could not read from its agent what the application consumed as the set's end stopped it: "+consumedAtStopUnread.Error())
			r.logger.Error("test-set is not judged: keploy could not read what its application consumed as it was stopped at the set's end; the run goes on to the next test-set ("+provenswitch.Env+"=1)",
				zap.String("test-set", testSetID), zap.Error(consumedAtStopUnread),
				zap.String("next_step", "check the agent's log for why the read failed, then run the test-set again"))
			testSetStatus = models.TestSetStatusInternalErr
			r.markNotJudged(testSetID)
		}
		if len(variablesUnjudged) > 0 && (testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed || notJudged != "") {
			notJudged = alsoNotJudged(notJudged, variablesUnjudgedReason(variablesUnjudged, variablesWhy[variablesUnjudged[0]]))
			r.logger.Error("test-set is not judged: keploy could not check what some of its tests captured against what their dependency calls carried; the run goes on to the next test-set",
				zap.String("test-set", testSetID),
				zap.Strings("tests", variablesUnjudged))
			testSetStatus = models.TestSetStatusInternalErr
			r.markNotJudged(testSetID)
		}
	}
	if conf.PostScript != "" {
		//Execute the Post-script after each test-set if provided
		r.logger.Info("Running Post-script", zap.String("script", conf.PostScript), zap.String("test-set", testSetID))
		err = r.executeScript(runTestSetCtx, conf.PostScript)
		if err != nil {
			return models.TestSetStatusFaultScript, fmt.Errorf("failed to execute post-script: %w", err)
		}
	}

	riskHigh, riskMed, riskLow := 0, 0, 0
	for _, tr := range testCaseResults {
		if tr.Status == models.TestStatusFailed && tr.Result.FailureInfo.Risk != models.None {
			switch tr.Result.FailureInfo.Risk {
			case models.High:
				riskHigh++
			case models.Medium:
				riskMed++
			case models.Low:
				riskLow++
			}
		}
	}

	// Checking errors for final iteration
	// Checking for errors in the loop
	if loopErr != nil && !errors.Is(loopErr, context.Canceled) {
		if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), loopErr); ok {
			testSetStatus = resolvedStatus
		} else {
			testSetStatus = models.TestSetStatusInternalErr
		}
	} else if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), nil); ok {
		testSetStatus = resolvedStatus
	}

	// A test whose unmatched calls could not all be read has no verdict, so its
	// set has none either: neither PASSED nor FAILED, and nothing a judge may
	// read as either (proven switch). A set already not judged for what its
	// tests consumed says both.
	// So has one whose capture could not be told (proven switch).
	if uncapturable, lost := testsKeployCannotCapture(testCaseResults); len(uncapturable) > 0 && (testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed || notJudged != "") {
		what := lost.NotJudgedText()
		notJudged = alsoNotJudged(notJudged, notCapturableReason(uncapturable, what))
		r.logger.Error("test-set is not judged: the application called what keploy cannot capture, a TLS client abandoned its handshake, or the Docker daemon did not answer keploy's read of a container a call reached, while some of its tests ran ("+provenswitch.Env+"=1)",
			zap.String("test-set", testSetID),
			zap.Strings("tests", uncapturable),
			zap.String("reason", what),
			zap.String("next_step", lost.NotJudgedRemedy()+", then run the test-set again"))
		testSetStatus = models.TestSetStatusInternalErr
		r.markNotJudged(testSetID)
	}
	if unreadCapture := testsWithUnreadCapture(testCaseResults); len(unreadCapture) > 0 && (testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed || notJudged != "") {
		notJudged = alsoNotJudged(notJudged, unreadCaptureReason(unreadCapture))
		r.logger.Error("test-set is not judged: keploy could not read whether its agent captured every call while some of its tests ran; the run goes on to the next test-set",
			zap.String("test-set", testSetID),
			zap.Strings("tests", unreadCapture),
			zap.String("next_step", "check the agent's log for why its count could not be read, then run the test-set again"))
		testSetStatus = models.TestSetStatusInternalErr
		r.markNotJudged(testSetID)
	}
	if heldOpen := testsWithStreamsHeldOpen(testCaseResults); len(heldOpen) > 0 && (testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed || notJudged != "") {
		// A test whose app kept its stream open on keepalives, past the
		// recording or short of it, has no verdict, so neither has its
		// set (row W1).
		notJudged = alsoNotJudged(notJudged, streamsHeldOpenReason(heldOpen))
		r.logger.Error("test-set is not judged: the app kept its stream open on keepalives in some of its tests, past the end of its recording or with the rest of it not coming; the run goes on to the next test-set",
			zap.String("test-set", testSetID),
			zap.Strings("tests", heldOpen),
			zap.String("next_step", streamsHeldOpenRemedy+", then run the test-set again"))
		testSetStatus = models.TestSetStatusInternalErr
		r.markNotJudged(testSetID)
	}
	if stalledAnswers := testsWithStalledAnswers(testCaseResults); len(stalledAnswers) > 0 && (testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed || notJudged != "") {
		// A test whose exchange ended with nothing of its answer to
		// compare has no verdict, so neither has its set (rule 15).
		notJudged = alsoNotJudged(notJudged, stalledAnswersReason(stalledAnswers))
		r.logger.Error("test-set is not judged: the app never answered some of its tests, sent nothing past an answer's headers, or never took a whole request, before keploy's stall bound passed or its keepalives showed it stalled; the run goes on to the next test-set",
			zap.String("test-set", testSetID),
			zap.Strings("tests", stalledAnswers),
			zap.String("next_step", stalledAnswersRemedy+", then run the test-set again"))
		testSetStatus = models.TestSetStatusInternalErr
		r.markNotJudged(testSetID)
	}
	unreadMisses := testsWithUnreadMisses(testCaseResults)
	if len(unreadMisses) > 0 && (testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed || notJudged != "") {
		notJudged = alsoNotJudged(notJudged, unreadMissesReason(unreadMisses))
		r.logger.Error("test-set is not judged: the calls some of its tests made that no mock answered could not all be read from keploy's agent; the run goes on to the next test-set",
			zap.String("test-set", testSetID),
			zap.Strings("tests", unreadMisses),
			zap.String("next_step", "check the agent's log for why the read was incomplete, then run the test-set again"))
		testSetStatus = models.TestSetStatusInternalErr
		r.markNotJudged(testSetID)
	}

	// A hook that could not read what the set's tests read (its recordings'
	// sealed values, say) says the set could not be checked, and why
	// (UncheckedHook). Under the proven switch a set with a verdict to give,
	// or one not judged already (an application that never got ready among
	// them), is not judged for it. Any other (the switch off, or a set that
	// halted, was stopped or faulted) keeps its status, and its report gives
	// the reason first (uncheckedFirst). Either way nothing of the run is
	// written back to the recording: no prune, no mappings.yaml, nothing
	// learned (below).
	unchecked := r.hookUnchecked(testSetID)
	uncheckedFirst := ""
	switch {
	case unchecked == "":
	case provenswitch.On() && (testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed || notJudged != "" || notReady != ""):
		notJudged = alsoNotJudged(notJudged, "not judged: "+unchecked)
		r.logger.Error("test-set is not judged: keploy could not read all its tests read, so it could not check them; the run goes on to the next test-set",
			zap.String("test-set", testSetID),
			zap.String("reason", unchecked))
		testSetStatus = models.TestSetStatusInternalErr
		r.markNotJudged(testSetID)
	default:
		uncheckedFirst = unchecked
		r.logger.Warn("keploy could not check every test of the test-set: it could not read all they read; the set keeps its status, and its report says why",
			zap.String("test-set", testSetID),
			zap.String("status", string(testSetStatus)),
			zap.String("reason", unchecked))
	}

	// An application that never got ready ran no test: the set is not judged,
	// saying why first (proven switch, readiness_proven.go).
	if notReady != "" {
		if notJudged == "" {
			notJudged = "not judged: " + notReady
		} else {
			notJudged = "not judged: " + notReady + "; " + notJudged
		}
		r.logger.Error("test-set is not judged: its application never got ready, so keploy fired none of its tests; the run goes on to the next test-set ("+provenswitch.Env+"=1)",
			zap.String("test-set", testSetID),
			zap.String("reason", notReady),
			zap.String("next_step", "start the application so that it listens where the set's first test dials (or answers its --health-url), see its logs in this report for why it did not, then run the test-set again"))
		testSetStatus = models.TestSetStatusInternalErr
		r.markNotJudged(testSetID)
		r.markNeverReady(testSetID)
	}

	appFailure := getLastAppErr()
	appLogs := appFailure.AppLogs

	// A set that stopped part-way is not a pass. scoredNothing below catches
	// only the all-or-nothing case: with at least one test scored, its four-zero
	// predicate is false, and because a failure would have taken the `default`
	// arm of the scoring switch, an all-passing or all-obsolete partial run kept
	// its PASSED status. The report then showed the gap and still called it
	// green — `Total tests: 4, passed 2, failed 0` — so the natural reading was
	// that two tests were skipped for some legitimate reason (#4618).
	//
	// APP_FAULT rather than FAILED for the same reason scoredNothing picks it:
	// no test failed, so calling the set FAILED sends someone looking for a
	// failing assertion that does not exist. It is also a status
	// shouldIncludeAppLogs carries logs for, and describeTestSetFailure now
	// gives this case its own wording rather than blaming an application that
	// is usually still running.
	//
	// Known cost, accepted: shouldAbortTestRun(APP_FAULT) is true off compose,
	// so the remaining test-sets are skipped rather than run. That is the same
	// consequence scoredNothing's doc refuses for an all-obsolete set, and the
	// reasoning differs because the situation does: an all-obsolete set ran and
	// answered, while this one stopped for a reason that is very unlikely to be
	// specific to this test-set — a replaced agent stays replaced. Continuing
	// would spend the remaining sets producing failures with the same cause.
	// The exit code is non-zero either way; what is lost is per-set detail.
	//
	// Guarded on PASSED so it only ever adds information: a set already carrying
	// a more specific verdict — FAILED, USER_ABORT, whatever resolveTestSetStatus
	// chose, or the INTERNAL_ERR a later insert failure sets — keeps it.
	// Whether the downgrade below actually fired — NOT merely whether the run
	// stopped early. The two are different for every early exit that already
	// had an honest diagnosis, and the reason string keys off this one.
	//
	// An app that OOM-kills after test 2 of 10 sets APP_HALTED via the app-error
	// channel and breaks the loop, so stoppedEarly is true there too. Handing
	// that to describeTestSetFailure replaced "application stopped during
	// replay" with "check keploy's own logs for why the run ended early" —
	// deleting the correct diagnosis for a plain app crash, on the most common
	// way APP_HALTED is reached with results present.
	partialRunDowngrade := stoppedEarly && testSetStatus == models.TestSetStatusPassed
	if stoppedEarly {
		// Logged whenever the run stopped early, not only when that changed the
		// status: a set that stopped early AND failed a test is still a set
		// whose remaining tests were never verified.
		const stoppedEarlyMsg = "test-set stopped before running every test; treat the tests missing from the totals as unverified, not as skipped"
		fields := []zap.Field{
			zap.String("test-set", testSetID),
			zap.Int("test-cases-loaded", testCasesCount),
			zap.Int("passed", success), zap.Int("failed", failure), zap.Int("obsolete", obsolete),
			zap.Bool("refusing-to-report-as-passed", partialRunDowngrade),
		}
		// A run with no agent has no application logs in its report: keploy
		// did not start the app. Its user stopping it is not an error, and
		// anything else that ended its context from above (ctx, which this
		// test set never cancels itself) has a cause to show.
		switch {
		case r.noAgent && testSetStatus == models.TestSetStatusUserAbort:
			r.logger.Info(stoppedEarlyMsg, append(fields,
				zap.String("next_step", "the run was stopped (Ctrl+C or SIGTERM): run it again to verify the rest"))...)
		case r.noAgent && ctx.Err() != nil:
			r.logger.Error(stoppedEarlyMsg, append(fields,
				zap.NamedError("cause", context.Cause(ctx)),
				zap.String("next_step", "the run's context ended before its tests did, for the cause given: give whatever runs keploy a longer deadline, or see why it ended the run"))...)
		case r.noAgent:
			r.logger.Error(stoppedEarlyMsg, append(fields,
				zap.String("next_step", "the run ended early rather than completing: the errors logged before this say why"))...)
		default:
			r.logger.Error(stoppedEarlyMsg, append(fields,
				zap.String("next_step", "the run ended early rather than completing: check the application logs in this report for why"))...)
		}
	}
	if partialRunDowngrade {
		testSetStatus = models.TestSetStatusFaultUserApp
	}

	// APP_FAULT rather than FAILED, because no test failed (see scoredNothing):
	// describeTestSetFailure already renders this exact case as "application
	// startup failed — check application logs", and shouldIncludeAppLogs
	// attaches those logs, which is what someone looking at an empty run needs.
	if scoredNothing(testSetStatus, testCasesCount, success, failure, ignored, obsolete) {
		r.logger.Error("test-set produced no results at all; refusing to report it as passed",
			zap.String("test-set", testSetID),
			zap.Int("test-cases-loaded", testCasesCount),
			zap.String("next_step", "nothing was verified by this run: check the application logs in this report, and — if a selected-test list is configured — that its names still match test cases in this set"))
		testSetStatus = models.TestSetStatusFaultUserApp
	}

	// A process the agent could not track -- the kernel refused it a place in
	// its PID map -- made calls that were not mocked and reached the real
	// dependency, so this test set's verdict was not reached against the
	// recording. FAILED when it would have passed; a status that already says
	// something went wrong keeps it. Either way the report says why first,
	// and what the tests consumed is no basis for the prune or for
	// mappings.yaml below: the mocks of those calls were consumed by nobody.
	//
	// A count that could not be read (pidMapVerdict.unverified) changes no
	// status, but holds the prune and mappings.yaml the same way: a consumed
	// set nothing shows is complete is no basis for deleting recorded mocks.
	pidVerdict := r.pidMapCaptureIncomplete(ctx, testSetID)
	captureIncomplete, captureUnverified := pidVerdict.incomplete, pidVerdict.unverified
	// Under the proven switch, what the agent lost after this set's last
	// per-test read is the set's own, charged to none of its tests: the set
	// fails with it, and it holds the prune and mappings.yaml, as above. The
	// next test set's tests count from here.
	late, lateErr := r.endCaptureLosses(context.WithoutCancel(ctx))
	// A call keploy cannot capture at all (decision 20) is no loss of what it
	// saw: below, it leaves the set without a verdict, never failed.
	lateJudged, lateNotJudged := late.Split()
	for _, t := range []string{lateJudged.Text(), realUncaptured} {
		if t == "" {
			continue
		}
		if captureIncomplete == "" {
			captureIncomplete = t
		} else {
			captureIncomplete += "; " + t
		}
	}
	if lateErr != nil && captureIncomplete == "" && captureUnverified == "" {
		captureUnverified = models.CaptureLossesUnverified(lateErr)
	}
	// A call keploy cannot capture at all after the set's last test
	// (decision 20): the set has no verdict, as one whose count is lost.
	if t := lateNotJudged.NotJudgedText(); t != "" && captureIncomplete == "" && captureUnverified == "" {
		captureUnverified = t
	}
	if captureIncomplete != "" && testSetStatus == models.TestSetStatusPassed {
		testSetStatus = models.TestSetStatusFailed
	}

	// A wrong answer demoted to OBSOLETE (switch off) has no verdict
	// (FailureInfo.MappingStale). A set that would read PASSED for it has
	// none either: never the silent pass it was (false-pass row 0). One that
	// has a verdict of its own keeps it, and its reason names the tests not
	// judged: a FAILED test failed whatever another's mapping says, a set that
	// stopped part-way (APP_FAULT, above) says so first, and so does one whose
	// calls reached a real dependency (captureIncomplete, just above). Placed
	// after those, so none is replaced by a remedy that re-records over it.
	staleMapping := testsWithStaleMapping(testCaseResults)
	staleWhy, staleOnly := "", ""
	if len(staleMapping) > 0 {
		if testSetStatus == models.TestSetStatusPassed || notJudged != "" {
			why := staleMappingReason(staleMapping)
			if notJudged == "" {
				staleOnly = why
			}
			notJudged = alsoNotJudged(notJudged, why)
			r.logger.Error("test-set is not judged: some of its tests answered differently from their recording while the dependency calls they made diverged from their recorded mapping, so keploy cannot tell a regression from a stale mapping; the run goes on to the next test-set",
				zap.String("test-set", testSetID),
				zap.Strings("tests", staleMapping),
				zap.String("next_step", models.MappingStaleRemedy))
			testSetStatus = models.TestSetStatusInternalErr
			r.markNotJudged(testSetID)
		} else {
			staleWhy = staleMappingNote(staleMapping)
			r.logger.Error("some tests of the test-set are not judged: they answered differently from their recording while the dependency calls they made diverged from their recorded mapping, so keploy cannot tell a regression from a stale mapping; the set keeps its own status",
				zap.String("test-set", testSetID),
				zap.String("status", string(testSetStatus)),
				zap.Strings("tests", staleMapping),
				zap.String("next_step", models.MappingStaleRemedy))
		}
	}
	// Under the proven switch the set is judged only if one agent process
	// answered for all of it (watchAgent). Read here, after the set's last
	// read of its agent (the losses after its last test, above): an agent
	// started again in its place answered those reads holding none of what
	// the one before saw, so the set has no verdict, as one whose evidence
	// could not be read. Before the capture check below, which then does not
	// take the set for unjudged on its capture count alone.
	if agentWatch != nil && (testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed || notJudged != "") {
		if rep, replaced := agentWatch.Replacement(); replaced {
			notJudged = alsoNotJudged(notJudged, agentReplacedReason(rep))
			r.logger.Error("test-set is not judged: keploy's agent was started again while the test-set ran, and what the agent before saw is lost; the run goes on to the next test-set ("+provenswitch.Env+"=1)",
				zap.String("test-set", testSetID),
				zap.String("agentProcess", rep.First),
				zap.String("startedAgainAs", rep.Next),
				zap.String("next_step", "find why the agent restarted (its container's restart count, an OOM kill, its log), then run the test-set again"))
			testSetStatus = models.TestSetStatusInternalErr
			r.markNotJudged(testSetID)
		}
	}
	// Under the proven switch a set whose capture could not be verified has
	// no verdict, as one whose evidence could not be read: neither PASSED nor
	// FAILED. With the switch off it keeps its status (no flag of keploy test
	// asks for more), with the holds below.
	// unverifiedNotJudged: the set is INTERNAL_ERR for this alone, which the
	// prune below names as capture_unverified, not as keploy's own loss.
	unverifiedNotJudged := false
	if captureUnverified != "" && provenswitch.On() &&
		(testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusFailed || notJudged != "") {
		unverifiedNotJudged = testSetStatus != models.TestSetStatusInternalErr
		notJudged = alsoNotJudged(notJudged, captureUnverified)
		r.logger.Error("test-set is not judged: keploy could not read whether its agent captured every call the application made; the run goes on to the next test-set ("+provenswitch.Env+"=1)",
			zap.String("test-set", testSetID), zap.String("reason", captureUnverified),
			zap.String("next_step", "check the agent's log for why its count could not be read, then run the test-set again"))
		testSetStatus = models.TestSetStatusInternalErr
		r.markNotJudged(testSetID)
	}
	// The report says what the capture check found (the founder rule: capture
	// is never silent). "incomplete" also fails the set, above; "unverified"
	// leaves the verdict as decided and only records that it is unconfirmed.
	var captureVerification *models.CaptureVerification
	switch {
	case captureIncomplete != "":
		captureVerification = &models.CaptureVerification{Status: models.CaptureIncompleteStatus, Reason: captureIncomplete}
	case captureUnverified != "":
		captureVerification = &models.CaptureVerification{Status: models.CaptureUnverifiedStatus, Reason: captureUnverified}
	}
	// What record's full round learned goes onto the recording before the
	// set is reported, and is said per test; a prune after it keeps it
	// (pruneKeeps). A round that could not keep it did not complete: its
	// verdict would be about recordings that lack what it served.
	//
	// A round a hook says could not read all its tests read (UncheckedHook)
	// learned on what it served unread: nothing of it is kept, and the
	// recording stays as recorded for a round that can.
	if r.learning() && r.instrument {
		if unchecked != "" {
			r.logger.Warn("keploy did not keep what this full round of the record learned: it could not read all the set's tests read, so what it learned may be about values it could not read",
				zap.String("testSetID", testSetID),
				zap.String("reason", unchecked))
		} else if err := r.persistLearned(runTestSetCtx, testSetID, allConsumedAcrossCycles, actualTestMockMappings); err != nil {
			utils.LogError(r.logger, err, "keploy did not complete the full round of this record: what its learn step learned was not kept", zap.String("testSetID", testSetID))
			testSetStatus = models.TestSetStatusInternalErr
		}
	}

	// Under the proven switch a set none of whose tests got a verdict, its
	// tests IGNORED, verified nothing, and is IGNORED, as a set the run skips
	// whole is, not PASSED. Last, so a status that says more (FAILED, not
	// judged, a partial run) keeps its place.
	if everyTestIgnored(testSetStatus, success, failure, obsolete, ignored) {
		r.logger.Warn("nothing in the test-set was verified: no test of it got a verdict (passed, failed or obsolete), and the ignored count says how many were ignored; it is reported IGNORED, not PASSED ("+provenswitch.Env+"=1)",
			zap.String("test-set", testSetID),
			zap.Int("ignored", ignored),
			zap.String("next_step", "its tests were skipped by the run's ignore list or set IGNORED by a before-test-result hook; the set is verified only when at least one of its tests runs and is not ignored"))
		testSetStatus = models.TestSetStatusIgnored
	}

	// A set not judged only for a stale mapping is no application's fault:
	// its report carries no application logs.
	if !shouldIncludeAppLogs(testSetStatus) || (staleOnly != "" && notJudged == staleOnly) {
		appLogs = ""
	} else if appLogs == "" {
		// Was gated on APP_HALTED, which made the fallback unreachable for every
		// other status that shouldIncludeAppLogs says carries logs. lastAppErr is
		// written only from the app-error channel, so a set that reaches APP_FAULT
		// WITHOUT the app erroring — a partial run, or a scoreless one where the
		// app never spoke — persisted a report whose own reason says "check
		// application logs in the app_logs field" next to an empty app_logs field.
		// shouldIncludeAppLogs already decides who gets logs; let it decide alone.
		//
		// Under the proven switch the set's end stopped the app keploy
		// started before this, and took its logs then: docker removes the
		// container it ran in once it stops.
		if appLogsBeforeStop != "" {
			appLogs = appLogsBeforeStop
		} else {
			logCtx, cancel := context.WithTimeout(context.WithoutCancel(runTestSetCtx), 10*time.Second)
			defer cancel()
			appLogs = r.instrumentation.GetRecentAppLogs(logCtx)
		}
	}
	testReport = &models.TestReport{
		Version:       models.GetVersion(),
		TestSet:       testSetID,
		Status:        string(testSetStatus),
		Total:         testCasesCount,
		Success:       success,
		Failure:       failure,
		Obsolete:      obsolete,
		Ignored:       ignored,
		Tests:         testCaseResults,
		TimeTaken:     timeTaken.String(),
		FailureReason: withUnchecked(uncheckedFirst, withCaptureIncomplete(captureIncomplete, describeTestSetFailure(testSetStatus, testCaseResults, runShape{stoppedEarly: stoppedEarly, downgradedFromPassed: partialRunDowngrade, notJudged: notJudged, realWhy: realWhy, unclaimedWhy: unclaimedReason, staleWhy: staleWhy}))),
		AppLogs:       appLogs,
		HighRisk:      riskHigh,
		MediumRisk:    riskMed,
		LowRisk:       riskLow,
		CmdUsed:       r.config.Test.CmdUsed,

		CaptureVerification: captureVerification,
		Real:                realRun,
		RealListed:          realListed,
		RealFailure:         realWhy,
		UnclaimedCalls:      unclaimed,
		AppNotStopped:       appNotStopped,
		Proven:              provenswitch.On(),
	}
	r.noteSetEnd(testReport)

	// final report should have reason for sudden stop of the test run so this should get canceled
	reportCtx := context.WithoutCancel(runTestSetCtx)
	err = r.reportDB.InsertReport(reportCtx, testRunID, testSetID, testReport)
	if err != nil {
		utils.LogError(r.logger, err, "failed to insert report")
		return models.TestSetStatusInternalErr, fmt.Errorf("failed to insert report")
	}

	err = utils.AddToGitIgnore(r.logger, r.config.Path, "/reports/")
	if err != nil {
		utils.LogError(r.logger, err, "failed to create .gitignore file")
	}

	// Preserve mocks and mappings for tests that were expected but not executed in this run
	if len(expectedTestMockMappings) > 0 {
		for expectedTestCaseID, expectedMocks := range expectedTestMockMappings {
			testExecuted := false
			for _, tc := range activeTestCases {
				if tc.Name == expectedTestCaseID {
					testExecuted = true
					break
				}
			}

			if !testExecuted {
				// Preserve its expected mocks so they aren't deleted by UpdateMocks
				for _, m := range expectedMocks {
					if _, exists := passingTotalConsumedMocks[m.Name]; !exists {
						passingTotalConsumedMocks[m.Name] = models.MockState{
							Name: m.Name,
							Kind: models.Kind(m.Kind),
						}
					}
				}
			}
		}
	}

	// Remove the mocks no test case consumed (when no base path is provided).
	//
	// Pruning keeps what the tests consumed and deletes the rest, so it is only
	// sound when a mock going unconsumed actually means the app didn't need it. A
	// run whose app could not reach its dependencies fails every test and consumes
	// nothing — pruning that would delete the recording's mocks because of an
	// infrastructure fault. shouldPrune holds them and lists the reasons.
	pruneEnabled := r.removeUnusedMocks() && r.instrument
	// INTERNAL_ERR: keploy lost part of the run, e.g. a result insert. Not
	// a set that is INTERNAL_ERR only for a capture count that could not be
	// read (under the proven switch, above): that is capture_unverified. Nor
	// one not judged only for a stale mapping: that is mapping_stale.
	internalErr := testSetStatus == models.TestSetStatusInternalErr && !unverifiedNotJudged && (staleOnly == "" || notJudged != staleOnly)
	// Read once: the prune and the mappings hold below decide on the same value.
	appUnreachable := anyAppConnectionError(testCaseResults)
	prune, pruneRefusal := shouldPrune(r.removeUnusedMocks(), r.instrument, pruneRun{
		allTestsGotAVerdict: allTestsGotAVerdict,
		internalErr:         internalErr,
		resultsComplete:     resultsComplete,
		noAnswer:            len(noAnswerTests) > 0,
		appUnreachable:      appUnreachable,
		captureIncomplete:   captureIncomplete != "" || testsLostCapture,
		captureUnverified:   captureUnverified != "",
		unchecked:           unchecked != "",
		mappingStale:        len(staleMapping) > 0,
		success:             success,
		failure:             failure,
		obsolete:            obsolete,
		preserveFailedMocks: r.config.Test.PreserveFailedMocks,
	})
	if pruneEnabled && !prune {
		fields := []zap.Field{
			zap.String("testSetID", testSetID),
			zap.String("reason", pruneRefusal),
			zap.Int("passed", success),
			zap.Int("failed", failure),
			zap.Int("obsolete", obsolete),
			zap.Int("testsToScore", toScore),
			zap.String("status", string(testSetStatus)),
			zap.Error(resultsErr),
		}
		if n := len(noAnswerTests); n > 0 {
			// One chronically unanswered test stops pruning for the whole set;
			// this names it. Capped so an outage does not log every test.
			fields = append(fields, zap.Int("noAnswerCount", n), zap.Strings("noAnswerTests", noAnswerTests[:min(n, 20)]))
		}
		r.logger.Warn("skipping mock pruning: this run's consumed-mock set is not trustworthy enough to delete against, so recorded mocks are preserved", fields...)
	}
	if prune {
		noisyTestCases := r.hookImpl.GetNoisyTestCaseNames(testSetID)
		if len(noisyTestCases) > 0 {
			added := retainNoisyTestCaseMocks(noisyTestCases, actualTestMockMappings, passingTotalConsumedMocks)
			if added > 0 {
				r.logger.Debug("preserved mocks used by noisy testcases from pruning",
					zap.String("testSetID", testSetID),
					zap.Int("noisyTestCaseCount", len(noisyTestCases)),
					zap.Int("additionalMocksKept", added))
			}
		}

		// Compute the startup-mock cutoff so UpdateMocks can exempt startup/init
		// mocks from deletion. "Startup mocks" are every mock recorded from app
		// boot up to and including the StartupMockTestCaseWindow-th test case;
		// mocks recorded before the cutoff survive pruning while still keeping
		// their per-test mappings for replay matching. With <= window test cases
		// the whole recording is startup, so pruneBefore is used as the cutoff to
		// retain everything (see startupMockCutoff).
		startupCutoffTime := startupMockCutoff(testCases, pruneBefore)

		err = r.mockDB.UpdateMocks(runTestSetCtx, testSetID, passingTotalConsumedMocks, pruneBefore, startupCutoffTime)
		if err != nil {
			utils.LogError(r.logger, err, "failed to delete unused mocks")
		}
	} else if r.config.Test.NoiseDetection() && r.instrument && !provenswitch.On() {
		// (Under the proven switch only record's full round writes what was
		// learned, above: a replay in CI writes no recording.)
		// --schema-noise-detection without --remove-unused-mocks: the learned
		// req_body_noise used to ride only inside UpdateMocks (the pruning
		// path), so detection alone learned noise and threw it away at exit.
		// Persist it through the prune-free path instead. We persist noise
		// from ALL consumed mocks (not just passing tests): the mock matched,
		// so the request-side drift it learned is valid even when the test
		// later failed on its response.
		type mockNoisePersister interface {
			PersistMockNoise(ctx context.Context, testSetID string, mockStates map[string]models.MockState) error
		}
		if p, ok := r.mockDB.(mockNoisePersister); ok {
			if err := p.PersistMockNoise(runTestSetCtx, testSetID, allConsumedAcrossCycles); err != nil {
				utils.LogError(r.logger, err, "failed to persist learned request-body noise onto mocks")
			}
		} else {
			r.logger.Debug("mockDB implementation does not support prune-free noise persistence; learned req_body_noise not persisted")
		}
	}

	// Test-mode mapping write semantics:
	//
	//   UpdateTestMapping=true  → write/merge mappings.yaml
	//                             (operator-driven update), with
	//                             the entries of the passing tests
	//                             only; a new file only when it
	//                             would map a test (see below).
	//   UpdateTestMapping=false → create-if-not-present: write only
	//                             when mappings.yaml is absent for
	//                             this test-set AND we have at least
	//                             one populated test→mocks entry.
	//                             Once a file exists, we leave it
	//                             alone — repeat test runs don't
	//                             churn it.
	//
	// Rationale: mappings.yaml is the artifact k8s-proxy autoreplay
	// (and other final-candidate analyses) consults to find which
	// mocks each test consumed. Operators who never set the flag
	// still need a usable file the first time test mode runs; gating
	// strictly on UpdateTestMapping leaves them without one. The
	// create-if-not-present default closes that gap without changing
	// behaviour for existing users who already have a mappings.yaml
	// they don't want overwritten — they can keep flag=false and the
	// existing file is preserved. To refresh the passing tests'
	// entries, flag=true.
	//
	// Independence from DisableMapping: the two flags express
	// orthogonal concerns. DisableMapping picks the replay-time mock
	// FILTERING strategy (timestamp-vs-index); the mapping WRITE is
	// a separate side-effect that records consumption. We don't gate
	// the write on the filter strategy.
	//
	// The non-emptiness guard on the create-if-not-present branch
	// matters because mappingDoc is built from actualTestMockMappings,
	// which is populated by
	// upsertActualTestMockMapping calls that depend on consumedMocks,
	// which is fetched from r.hookImpl.GetConsumedMocks INSIDE
	// `if r.instrument` blocks (lines 1453, 1884). In non-instrument
	// modes (e.g. k8s-proxy autoreplay) consumedMocks stays empty,
	// so without this guard the first run would create an empty
	// mappings.yaml and every subsequent run would skip the create
	// branch (file exists), leaving autoreplay permanently without
	// the mappings the feature relies on. UpdateTestMapping=true
	// still writes an empty file when explicitly requested and some
	// test passed — that matches the operator intent of "force a
	// refresh"; it creates none from a run where no test passed.
	// Kept as it was: the run passed tests, and found none of them
	// consumed a mock, which is what the empty file says. It maps no
	// test, so each is served its recorded window as with no file,
	// and every later flag run merges into it; only a later default
	// run's create is skipped, as for any file that exists.
	//
	// No write may leave a loaded test out of the file for good because the
	// run lost it, as no default run adds to a file that exists. A run that did
	// not score every loaded test writes nothing: no create, no StoreMappings
	// merge, no backfill. A run where a test got no answer, was refused or lost
	// its result has no entry for that test, so by default it must not create
	// the file, but may backfill one that exists. --update-test-mapping writes
	// it either way: mapdb.Insert replaces only this run's entries, so the next
	// such run adds the missing test (MergeStartupMockNames keeps an unmapped
	// test on the time window).
	//
	// Only a passing test's entry is written (see mapping_verdicts.go),
	// decided on the verdicts as the report holds them, after the
	// BeforeTestResult hook. The document holds no entry for any other test, so
	// it keeps the one it has or, with none, stays on its recorded window. A
	// new file therefore lacks a test that did not pass, and keeps lacking it
	// under the default flag; that is not the loss the rule above guards
	// against. It is what the test is owed: the mocks of its recorded window.
	// Holding the create instead would leave every test of a set with one
	// failure with no mapping at all, where a set recorded without a
	// mappings.yaml gets one from its first replay (enterprise's auto-replay,
	// for one).
	passing := passingTests(testCaseResults, consumptionUnread, obsoleteShaped, passedMissShaped(testCaseResults, leftRecordedUnconsumed))
	mappingDoc, keptRecorded := passingEntriesOf(actualTestMockMappings, passing)
	markWrittenByKeployTest(mappingDoc)
	// Whether a boot consumer's fetch was caught by the set's setup drain or
	// by a test's drain is timing: a startup section holding the app's
	// startup traffic is written in recorded order, so two runs write it
	// alike. One with none is written as before.
	if carryAttr.bootListed {
		sortByRecordedTime(mappingDoc.Startup)
	}
	if len(passing) == 0 {
		// The boot traffic belongs to no test, and a run where no test passed
		// vouches for it no more than for the tests: the section on disk stays.
		mappingDoc.Startup = nil
	}
	mappingExists := sync.OnceValues(func() (bool, error) { return r.mappingDB.Exists(ctx, testSetID) })
	holdMappings := ""
	// holdQuietly: a hold that is the expected outcome, logged at Debug.
	holdQuietly := false
	// Whether the run had anything to write at all, before the verdicts took
	// their part: what the holds below are about.
	wouldWrite := r.mappingDB != nil && (r.config.Test.UpdateTestMapping ||
		len(actualTestMockMappings.TestCases) > 0 || len(actualTestMockMappings.Startup) > 0)
	switch {
	case !wouldWrite:
	case provenswitch.On():
		// Under the proven switch only record writes mappings.yaml (design
		// v14 §5.2): a replay never changes what a test owns.
		holdMappings, holdQuietly = "under KEPLOY_PROVEN only a record writes mappings.yaml", true
		if r.config.Test.UpdateTestMapping {
			r.noteIgnored("updateTestMapping (--update-test-mapping)", "only a record writes mappings.yaml")
		}
	case pidVerdict.incomplete != "":
		// Whatever the flag: a call of an untracked process reached the real
		// dependency, so the mock recorded for it is in no test's entry. A
		// file created from that lacks it for good, and --update-test-mapping
		// would put the short entries in place of the complete ones. First,
		// as in the prune's reasons: it is positive evidence of a loss.
		holdMappings = "the agent could not track every process the application ran, so the calls of an untracked one are in no test's entry"
	case captureIncomplete != "" || testsLostCapture:
		// The same for a call keploy lost (proven switch).
		holdMappings = "keploy could not capture every call the application made, so a lost call's mock is in no test's entry"
	case unchecked != "":
		// A hook could not read all the set's tests read (UncheckedHook): a
		// mock served unread may have matched no call, and is then in no
		// test's entry.
		holdMappings = "keploy could not read all the set's tests read, so a mock it served unread may be in no test's entry: " + unchecked
	case !allTestsGotAVerdict:
		holdMappings = "not every loaded test got a verdict"
	case pidVerdict.unverified != "":
		// The same, unproven either way: the agent's count could not be read.
		holdMappings = "keploy could not read whether the agent tracked every process the application ran, so the calls of an untracked one may be in no test's entry"
	case captureUnverified != "":
		holdMappings = "keploy could not verify that the agent captured every call the application made, so a call it did not see may be in no test's entry"
	case !r.config.Test.UpdateTestMapping && (len(noAnswerTests) > 0 || internalErr || !resultsComplete || appUnreachable):
		if exists, existsErr := mappingExists(); existsErr != nil || !exists {
			holdMappings = "a test got no answer, was refused or lost its result, and a new file would lack it for good"
		}
	case len(mappingDoc.TestCases) == 0 && (len(keptRecorded) > 0 || len(passing) == 0 || !r.config.Test.UpdateTestMapping):
		// A new file would map no test, and, existing, would stop every later
		// default run from creating the one that does. Into a file that exists
		// the run goes on: the startup backfill, or the flag's write, which
		// leaves every entry on disk as it is (with nothing to write, it does
		// not touch the file). (A flag run where tests passed and none consumed
		// anything writes as it always has.)
		if exists, existsErr := mappingExists(); existsErr != nil || !exists {
			switch {
			case len(keptRecorded) > 0:
				holdMappings = "no test that consumed a mock counts as passing, so a new file would map no test; a later run in which one passes creates it"
			case len(passing) == 0:
				holdMappings = "no test passed, so a new file would map no test; a later run in which a test that consumes a mock passes creates it"
			default:
				holdMappings = "no test consumed a mock, so a new file would map no test"
				holdQuietly = true
			}
		}
	}
	shouldWriteMappings := r.config.Test.UpdateTestMapping && holdMappings == ""
	if !r.config.Test.UpdateTestMapping && holdMappings == "" && r.mappingDB != nil && len(mappingDoc.TestCases) > 0 {
		exists, existsErr := mappingExists()
		if existsErr != nil {
			r.logger.Debug("Skipping create-if-not-present mappings.yaml write — file-existence check failed; treating as 'exists' to avoid clobbering",
				zap.String("testSetID", testSetID),
				zap.Error(existsErr))
		} else if !exists {
			shouldWriteMappings = true
		}
	}

	// See backfillStartupSection: writes a STARTUP-ONLY document so the
	// operator's per-test mappings are never rewritten by a subset run. Only
	// into a file that exists: one it created would map no test (the hold
	// above names that case).
	if holdMappings != "" {
		// Into a file that exists, the default flag would only have backfilled
		// the startup section, so a subset run on a mapped set stays quiet.
		logHold := r.logger.Warn
		if holdQuietly {
			logHold = r.logger.Debug
		} else if !r.config.Test.UpdateTestMapping {
			if exists, existsErr := mappingExists(); existsErr == nil && exists {
				logHold = r.logger.Debug
			}
		}
		fields := []zap.Field{zap.String("testSetID", testSetID), zap.String("reason", holdMappings)}
		if n := len(keptRecorded); n > 0 && len(mappingDoc.TestCases) == 0 {
			fields = append(fields, zap.Int("count", n), zap.Strings("testsNotCountedAsPassing", keptRecorded[:min(n, 20)]))
		}
		logHold("not writing mappings.yaml", fields...)
	} else if !shouldWriteMappings && r.mappingDB != nil && len(mappingDoc.Startup) > 0 {
		if exists, existsErr := mappingExists(); existsErr == nil && exists {
			r.backfillStartupSection(ctx, testSetID, mappingDoc)
		}
	}

	if shouldWriteMappings {
		// Only a set that holds connection failures can have mapping entries
		// for them to keep: every other set is written as before, with no
		// second read of its mapping file.
		if holdsConnFailures(mockKindByName) {
			onDisk, _, err := r.mappingDB.Get(ctx, testSetID)
			var onDiskStartup []models.MockEntry
			if err == nil {
				onDiskStartup, err = r.mappingDB.GetStartup(ctx, testSetID)
			}
			if err == nil {
				keepConnFailureMappings(mappingDoc, onDisk, onDiskStartup, mockKindByName)
			} else {
				r.logger.Debug("could not read the mapping file to keep its connection-failure entries", zap.String("testSetID", testSetID), zap.Error(err))
			}
		}
		logKept := func() {
			if n := len(keptRecorded); n > 0 {
				// Named, capped as the unanswered tests are: an auto-replay that
				// failed tests must say their mappings were left as recorded.
				r.logger.Info("left the mapping of the tests not counted as passing as recorded (their entry, or none: their recorded window): only a passing test's consumption updates mappings.yaml",
					zap.String("testSetID", testSetID),
					zap.Int("count", n),
					zap.Strings("tests", keptRecorded[:min(n, 20)]))
			}
		}
		// A document with no entry and no startup section has nothing to put
		// into a file that exists: it is left as it is, not rewritten. (Into
		// none, a flag run where tests passed and none consumed anything
		// writes an empty file, as it always has.)
		nothingToWrite := false
		if len(mappingDoc.TestCases) == 0 && len(mappingDoc.Startup) == 0 {
			if exists, existsErr := mappingExists(); existsErr == nil && exists {
				nothingToWrite = true
			}
		}
		if nothingToWrite {
			logKept()
			r.logger.Debug("mappings.yaml left as it is: this run has no passing test's entry and no startup section to write",
				zap.String("testSetID", testSetID))
		} else if err := r.StoreMappings(ctx, mappingDoc); err != nil {
			r.logger.Error("Error saving test-mock mappings to YAML file", zap.Error(err))
		} else {
			logKept()
			// startupMocks is reported at INFO, not DEBUG, on purpose: it is the
			// only signal that the startup section was actually captured. Every
			// other trace of it (mapdb.GetStartup, determineMockingStrategy) is
			// DEBUG, and CI lanes do not run the replay at debug level — so
			// without this a run that silently recorded ZERO startup mocks looks
			// identical to one that recorded them correctly, and the whole
			// feature is unobservable from a green pipeline.
			r.logger.Info("Successfully saved test-mock mappings",
				zap.String("testSetID", testSetID),
				zap.Int("numTests", len(mappingDoc.TestCases)),
				zap.Int("startupMocks", len(mappingDoc.Startup)))
		}
	}

	// TODO Need to decide on whether to use global variable or not
	//
	// An IGNORED set (everyTestIgnored) is shown in the run's summary as a set
	// the run skips whole is.
	verdict := TestReportVerdict{
		total:     testReport.Total,
		failed:    testReport.Failure,
		passed:    testReport.Success,
		obsolete:  testReport.Obsolete,
		ignored:   testReport.Ignored,
		status:    testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusIgnored,
		duration:  timeTaken,
		timeTaken: timeTaken.String(),
	}

	r.completeTestReportMu.Lock()
	r.completeTestReport[testSetID] = verdict
	r.totalTests += testReport.Total
	r.totalTestPassed += testReport.Success
	r.totalTestFailed += testReport.Failure
	r.totalTestObsolete += testReport.Obsolete
	r.totalTestIgnored += testReport.Ignored
	r.totalTestTimeTaken += timeTaken
	r.completeTestReportMu.Unlock()

	timeTakenStr := timeWithUnits(timeTaken)

	// An IGNORED set here is one whose tests were each ignored (everyTestIgnored):
	// it is printed as it was when it was reported PASSED.
	if testSetStatus == models.TestSetStatusFailed || testSetStatus == models.TestSetStatusPassed || testSetStatus == models.TestSetStatusIgnored {
		if r.config.JSONOutput {
			if err := utils.NewJSONWriter(true).Write(testReport); err != nil {
				utils.LogError(r.logger, err, "failed to print json testrun summary")
			}
		} else {

			if !r.config.DisableANSI {
				if testSetStatus == models.TestSetStatusFailed {
					pp.SetColorScheme(models.GetFailingColorScheme())
				} else {
					pp.SetColorScheme(models.GetPassingColorScheme())
				}

				summaryFormat := "\n <=========================================> \n" +
					"  TESTRUN SUMMARY. For test-set: %s\n" +
					"\tTotal tests:        %s\n" +
					"\tTotal test passed:  %s\n" +
					"\tTotal test failed:  %s\n"

				args := []interface{}{testReport.TestSet, testReport.Total, testReport.Success, testReport.Failure}
				if testReport.Obsolete > 0 {
					summaryFormat += "\tTotal test obsolete: %s\n"
					args = append(args, testReport.Obsolete)
				}

				if testReport.Ignored > 0 {
					summaryFormat += "\tTotal test ignored: %s\n"
					args = append(args, testReport.Ignored)
				}

				summaryFormat += "\tTime Taken:         %s\n <=========================================> \n\n"
				args = append(args, timeTakenStr)

				if _, err := pp.Printf(summaryFormat, args...); err != nil {
					utils.LogError(r.logger, err, "failed to print testrun summary")
				}

			} else {
				var sb strings.Builder
				sb.WriteString("\n <=========================================> \n")
				sb.WriteString(fmt.Sprintf("  TESTRUN SUMMARY. For test-set: %s\n", testReport.TestSet))
				sb.WriteString(fmt.Sprintf("\tTotal tests:        %d\n", testReport.Total))
				sb.WriteString(fmt.Sprintf("\tTotal test passed:  %d\n", testReport.Success))
				sb.WriteString(fmt.Sprintf("\tTotal test failed:  %d\n", testReport.Failure))
				if testReport.Obsolete > 0 {
					sb.WriteString(fmt.Sprintf("\tTotal test obsolete: %d\n", testReport.Obsolete))
				}

				if testReport.Ignored > 0 {
					sb.WriteString(fmt.Sprintf("\tTotal test ignored: %d\n", testReport.Ignored))
				}

				sb.WriteString(fmt.Sprintf("\tTime Taken:         %s\n", timeTakenStr))
				sb.WriteString(" <=========================================> \n\n")

				fmt.Print(sb.String())
			}
		}
	}

	r.telemetry.TestSetRun(testReport.Success, testReport.Failure, testSetID, string(testSetStatus))

	// keploy test never writes config.yaml's template. It holds the values
	// as RECORDED: every run starts from them, and a capture takes the run's
	// own value from the app's answer as the run goes. A run's values belong
	// to that run; written back, the next run would start from a value the
	// recording never held. --update-template is kept, doing nothing, so a
	// keploy.yml or a CI line that sets it does not break.
	if r.config.Test.UpdateTemplate && len(conf.Template) > 0 {
		r.logger.Warn("--update-template no longer writes this run's template values into config.yaml: config.yaml keeps the recorded values, and {{capture ...}} placeholders take each run's values from the app's answers as the run goes",
			zap.String("testset", testSetID),
			zap.String("next_step", "remove --update-template (or test.updateTemplate) from your command or keploy.yml"))
	}

	// In Docker Compose mode, RunTestSet's defer block stops the agent container.
	// We must call AfterTestRun HERE (before defer fires) while the agent is alive.
	if r.isLastTestSet && r.instrument && cmdType == utils.DockerCompose {
		r.afterTestRunCalled = true
		if hookErr := r.hookImpl.AfterTestRun(ctx, r.testRunID, r.testRunTestSets, models.TestCoverage{}); hookErr != nil {
			utils.LogError(r.logger, hookErr, "failed to execute after test run hook")
		}
	}

	// Merge mock NAMES (not the count) into the run-level distinct set so
	// duplicates across test sets aren't double-counted. allConsumedAcrossCycles
	// is the cross-cycle union (folded at each rewind and once after the loop),
	// so names consumed only in an earlier retry cycle aren't lost from the
	// Mocks-Consumed telemetry.
	r.completeTestReportMu.Lock()
	for name := range allConsumedAcrossCycles {
		r.consumedMockNames[name] = struct{}{}
	}
	r.completeTestReportMu.Unlock()

	return testSetStatus, nil
}

func (r *Replayer) GetMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) (filtered, unfiltered []*models.Mock, err error) {
	if reader, ok := r.mockDB.(pkg.TestSetMocksReader); ok {
		set, err := r.getTestSetMocks(ctx, reader, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed)
		return set.Filtered, set.Unfiltered, err
	}
	filtered, err = r.mockDB.GetFilteredMocks(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		utils.LogError(r.logger, err, "failed to get filtered mocks")
		return nil, nil, err
	}
	unfiltered, err = r.mockDB.GetUnFilteredMocks(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		utils.LogError(r.logger, err, "failed to get unfiltered mocks")
		return nil, nil, err
	}
	return filtered, unfiltered, err
}

// getTestSetMocks reads both of the test set's pools, and its session pool
// before the mapping prune, in one pass over its mock file.
func (r *Replayer) getTestSetMocks(ctx context.Context, reader pkg.TestSetMocksReader, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) (models.TestSetMocks, error) {
	set, err := reader.GetTestSetMocks(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		// The pass that failed is the one the per-test pool was read in.
		utils.LogError(r.logger, err, "failed to get filtered mocks")
		return models.TestSetMocks{}, err
	}
	return set, nil
}

// loadTestSetMocks is GetMocks over the run's whole window, plus the report's
// mock lookup: the test set's per-test and session mocks as recorded.
// RunTestSet used to read the set's mock file once per pool and once more for
// the lookup, three full decodes before the first test. A store that reads
// every pool in one pass (pkg.TestSetMocksReader) gives the lookup from that
// read: the per-test candidates and the session pool before the mapping prune
// and the window filter. A store that reads one pool per call gives the
// per-test half from the pool it loaded, which the prune never takes a mock a
// running test expects from, and the session half from the session pool it
// loaded when the prune had nothing to drop, else from a read of its own.
//
// The lookup is built here, before AfterGetMocks may change the mocks in place
// (a mutator decrypts them, for one), so it describes them as they are on disk.
// skipped is the documents the store's decoders skipped (models.TestSetMocks
// Skipped and SkippedWindowed); none from a store that reads one pool per
// call.
func (r *Replayer) loadTestSetMocks(ctx context.Context, testSetID string, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) (filtered, unfiltered []*models.Mock, lookup map[string]mockDisplayInfo, skipped skippedDocs, err error) {
	reader, ok := r.mockDB.(pkg.TestSetMocksReader)
	if !ok {
		filtered, unfiltered, err = r.GetMocks(ctx, testSetID, models.BaseTime, time.Now(), mocksThatHaveMappings, mocksWeNeed)
		if err != nil {
			return nil, nil, nil, skippedDocs{}, err
		}
		allSession := unfiltered
		if len(mocksThatHaveMappings) > 0 {
			allSession, err = r.mockDB.GetUnFilteredMocks(ctx, testSetID, models.BaseTime, time.Now(), nil, nil)
			if err != nil {
				r.logger.Debug("failed to read the session mocks for the report's mock lookup; the report will not describe them",
					zap.String("testSetID", testSetID), zap.Error(err))
				allSession = nil
			}
		}
		return filtered, unfiltered, newMockLookup(filtered, allSession), skippedDocs{}, nil
	}
	set, err := r.getTestSetMocks(ctx, reader, testSetID, models.BaseTime, time.Now(), mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		return nil, nil, nil, skippedDocs{}, err
	}
	return set.Filtered, set.Unfiltered, newMockLookup(set.AllPerTest, set.AllSession), skippedDocs{kinds: set.Skipped, windowed: set.SkippedWindowed}, nil
}

// skippedDocs is the documents of a set's mock file the store's decoders
// skipped: their kinds by name, and those whose request time could be read as
// test windows see them (models.TestSetMocks).
type skippedDocs struct {
	kinds    map[string]models.Kind
	windowed []models.WindowedMock
}

// readMockLookup reads the report's mock lookup for a test set whose mocks
// RunTestSet does not load: its per-test and session mocks, in one read when
// the store can, else in one read per pool. A read that fails leaves its pool
// out of the lookup.
func (r *Replayer) readMockLookup(ctx context.Context, testSetID string) map[string]mockDisplayInfo {
	if r.mockDB == nil {
		return map[string]mockDisplayInfo{}
	}
	logFailed := func(pool string, err error) {
		r.logger.Debug("failed to read the "+pool+" mocks for the report's mock lookup; the report will not describe them",
			zap.String("testSetID", testSetID), zap.Error(err))
	}
	if reader, ok := r.mockDB.(pkg.TestSetMocksReader); ok {
		set, err := reader.GetTestSetMocks(ctx, testSetID, models.BaseTime, time.Now(), nil, nil)
		if err != nil {
			logFailed("per-test and session", err)
			return map[string]mockDisplayInfo{}
		}
		return newMockLookup(set.AllPerTest, set.AllSession)
	}
	// No window: the per-test reader then filters nothing out, so the lookup
	// gets every per-test mock as recorded, as AllPerTest gives it above.
	perTest, err := r.mockDB.GetFilteredMocks(ctx, testSetID, time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		logFailed("per-test", err)
		perTest = nil
	}
	session, err := r.mockDB.GetUnFilteredMocks(ctx, testSetID, models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		logFailed("session", err)
		session = nil
	}
	return newMockLookup(perTest, session)
}

// newMockLookup builds the report's mock lookup from a test set's per-test and
// session mocks. A name in both, which the gob store's PostgresV2 mocks are,
// keeps the session mock's entry, as the lookup did when it held only those.
func newMockLookup(perTest, session []*models.Mock) map[string]mockDisplayInfo {
	lookup := make(map[string]mockDisplayInfo, len(perTest)+len(session))
	addMockDisplayInfo(lookup, perTest)
	addMockDisplayInfo(lookup, session)
	return lookup
}

// addMockDisplayInfo adds each mock's display info to the lookup, keyed by
// name; a later mock of the same name replaces an earlier one.
func addMockDisplayInfo(lookup map[string]mockDisplayInfo, mocks []*models.Mock) {
	for _, mock := range mocks {
		lookup[mock.Name] = mockDisplayInfo{
			summary:  models.MockSummaryFromSpec(mock),
			protocol: string(mock.Kind),
			target:   mockTargetFromSpec(mock),
		}
	}
}

// consumedForAgent is the consumed history to send with filter params: only the
// per-test region's entries once the agent has said it reads no others
// (pkg.ConsumedScopeReader), the whole of it until then. Agents from v3.0.0-beta1
// through v3.3.22 also applied the history to the session pool, so only that
// answer, never this client's own view of the agent, may narrow it.
func (r *Replayer) consumedForAgent(total map[string]models.MockState, region map[string]struct{}) map[string]models.MockState {
	if s, ok := r.instrumentation.(pkg.ConsumedScopeReader); ok && s.AgentReadsConsumedPerTestOnly() {
		return pkg.ConsumedForAgent(total, region)
	}
	return total
}

// firstRecordedTestStart returns the request time of the earliest RECORDED test
// in the set, of any kind that opens a window (models.RecordedWindow: an HTTP
// or a gRPC test case). It is the earliest of them all, not the first
// case's: an HTTP case's request time was all that was read once, so a set of
// gRPC cases had no cutoff (its staging read the whole recording), and a set
// whose first test was a gRPC one cut at its first HTTP test, too late. A case
// of neither kind counts its HTTP request time, as every case did before. A
// case with no request time contributes nothing rather than pinning the
// cutoff to the zero time.
func firstRecordedTestStart(testCases []*models.TestCase) time.Time {
	var first time.Time
	for _, tc := range testCases {
		if tc == nil {
			continue
		}
		ts := models.RecordedWindow(tc).Start
		if ts.IsZero() && tc.Kind != models.HTTP && tc.Kind != models.GRPC_EXPORT {
			ts = tc.HTTPReq.Timestamp
		}
		if !ts.IsZero() && (first.IsZero() || ts.Before(first)) {
			first = ts
		}
	}
	return first
}

// recordedSetShape is what the staging call tells the agent about the set's
// recording as a whole. Per-test calls send the zero value.
//
//   - firstTestStart is the request time of the EARLIEST RECORDED test; the
//     agent seeds the startup-init cutoff from it instead of from whichever
//     test fires first.
//   - windows is the window of EVERY recorded test, selected or not; the agent
//     releases traffic the test windows do not pace by themselves (server push)
//     at the recorded window it belongs to.
type recordedSetShape struct {
	firstTestStart time.Time
	windows        []models.TestWindow
}

func recordedSetShapeOf(testCases []*models.TestCase) recordedSetShape {
	return recordedSetShape{
		firstTestStart: firstRecordedTestStart(testCases),
		windows:        recordedTestWindows(testCases),
	}
}

// recordedSetShape is the shape a staging call sends for testSetID: that of its
// test cases, with the first test moved back to the earliest test the set ever
// recorded when the TestDB keeps it (RecordedStartReader). The agent stages
// what was recorded before that test as the app's startup traffic, and every
// test of the run reads the same startup mocks, so a late first test would
// hand them most of the recording.
func (r *Replayer) recordedSetShape(ctx context.Context, testSetID string, testCases []*models.TestCase) recordedSetShape {
	shape := recordedSetShapeOf(testCases)
	reader, ok := r.testDB.(RecordedStartReader)
	if !ok {
		return shape
	}
	recorded, err := reader.RecordedFirstTestStart(ctx, testSetID)
	if err != nil {
		r.logger.Warn("could not read when the test set's first test was recorded; its startup traffic is taken from the test cases it holds now",
			zap.String("testSetID", testSetID), zap.Error(err))
		return shape
	}
	if !recorded.IsZero() && (shape.firstTestStart.IsZero() || recorded.Before(shape.firstTestStart)) {
		shape.firstTestStart = recorded
	}
	return shape
}

// recordedTestWindows returns the window each recorded test opens on the agent
// when it runs — the same [req, res] pair the per-test SendMockFilterParamsToAgent
// call sends (HTTP and gRPC; other kinds open none) — for every test case of the
// set, including those this run does not select. A case with no request time
// opens no window and is left out.
func recordedTestWindows(testCases []*models.TestCase) []models.TestWindow {
	out := make([]models.TestWindow, 0, len(testCases))
	for _, tc := range testCases {
		start, end := testWindowOf(tc)
		if start.IsZero() {
			continue
		}
		out = append(out, models.TestWindow{TestCase: tc.Name, Start: start, End: end})
	}
	return out
}

// testWindowOf is the [req, res] window the per-test agent call opens for tc
// (models.RecordedWindow).
func testWindowOf(tc *models.TestCase) (time.Time, time.Time) {
	w := models.RecordedWindow(tc)
	return w.Start, w.End
}

// SendMockFilterParamsToAgent sends filtering parameters to agent instead of
// sending filtered mocks. shape is only meaningful on the set's staging call
// (see recordedSetShape); pass the zero value on per-test calls.
func (r *Replayer) SendMockFilterParamsToAgent(ctx context.Context, expectedMockMapping []string, afterTime, beforeTime time.Time, totalConsumedMocks map[string]models.MockState, useMappingBased bool, shape recordedSetShape, vars ...models.VariableBinding) error {
	if !r.instrument {
		r.logger.Debug("Keploy will not filter and set mocks when base path is provided", zap.String("base path", r.config.Test.BasePath))
		return nil
	}

	// Build filter parameters. Default to strict=true when r.config is
	// nil (unit tests, embedders) — matches the shipped default in
	// config.Test.StrictMockWindow. The env override applies at the
	// agent so KEPLOY_STRICT_MOCK_WINDOW=0 still opts out without
	// touching code.
	strictMockWindow := true
	if r.config != nil {
		strictMockWindow = r.config.Test.StrictMockWindow
	}
	// EXPERIMENTAL, default OFF: when KEPLOY_AGENT_OWNS_CONSUMED is set, the
	// agent applies filterOutDeleted from its own persistent consumption history
	// instead of us re-sending the ever-growing TotalConsumedMocks map every
	// testcase (O(testcases^2) marshaling). We then send a nil map to eliminate
	// that cost. Off by default so behaviour is byte-identical until proven
	// equivalent by the golden-reference harness.
	agentOwnsConsumed := os.Getenv("KEPLOY_AGENT_OWNS_CONSUMED") == "1" ||
		strings.EqualFold(os.Getenv("KEPLOY_AGENT_OWNS_CONSUMED"), "true")
	consumedForAgent := totalConsumedMocks
	if agentOwnsConsumed {
		consumedForAgent = nil
	}
	// A replacement agent has no history to own, and never will for the mocks
	// consumed before it started. Send ours instead of letting it filter from
	// its own (empty) record.
	//
	// STICKY, not just this call. The flag is evaluated per request
	// (pkg/service/agent/agent.go), so handing the replacement one seeded call
	// and then reverting to AgentOwnsConsumed on every later per-test call would
	// put it straight back to filtering against an empty map — already-consumed
	// mocks re-served for the rest of the run. Once an agent has been replaced,
	// the CLI's map is the only complete record there is.
	if agentOwnsConsumed && r.agentConsumedHistoryUnusable() {
		agentOwnsConsumed = false
		consumedForAgent = totalConsumedMocks
		r.logger.Debug("sending the CLI's consumed-mock history: KEPLOY_AGENT_OWNS_CONSUMED is set but "+
			"the agent's own history is no longer authoritative for this run — either it was replaced "+
			"mid-run, or a retry cycle rewound the CLI's map and nothing rewinds the agent's",
			zap.Int("consumed", len(totalConsumedMocks)))
	}
	params := models.MockFilterParams{
		AfterTime:              afterTime,
		BeforeTime:             beforeTime,
		FirstRecordedTestStart: shape.firstTestStart,
		RecordedWindows:        shape.windows,
		MockMapping:            expectedMockMapping,
		UseMappingBased:        useMappingBased,
		AgentOwnsConsumed:      agentOwnsConsumed,
		TotalConsumedMocks:     consumedForAgent,
		StrictMockWindow:       strictMockWindow,
		Variables:              vars,
	}
	// Under the proven switch the set's staging call (no mapping, from
	// BaseTime) stages the set once, and the agent then serves its pool
	// (exact.go).
	if r.exact != nil && afterTime.Equal(models.BaseTime) && len(expectedMockMapping) == 0 {
		params.Exact = &models.ExactServing{Stage: true, Pool: append([]string(nil), r.exact.pool...), Worker: append([]models.MockEntry(nil), r.exact.worker...)}
	}

	// Send parameters to agent for filtering and mock updates
	err := r.instrumentation.UpdateMockParams(ctx, params)
	if err != nil {
		utils.LogError(r.logger, err, "failed to update mock parameters on agent")
		return err
	}

	r.logger.Debug("Successfully sent mock filter parameters to agent",
		zap.Bool("useMappingBased", useMappingBased),
		zap.Int("mockMappingCount", len(expectedMockMapping)))

	return nil
}

// mockStatsProbeTimeout bounds each /mock/stats read in the agent-replacement
// guard.
//
// The shared AgentClient is built with http.Client{} and no timeout, so without
// a per-call deadline a WEDGED agent — as opposed to a dead one, which fails
// fast — blocks this guard for as long as the test-set context lives. A
// pre-first-test gate that can hang is worse than the defect it guards: the run
// stops with no verdict at all instead of proceeding. pkg/service/mock/replay.go
// bounds its own served-mocks poll for exactly this reason.
//
// Generous for what it covers (one local HTTP round trip to the agent) so a
// merely busy agent is never mistaken for a wedged one.
// vars, not consts, so tests can shrink them rather than wait them out.
var (
	mockStatsProbeTimeout = 10 * time.Second

	// agentRepairTimeout bounds the whole re-registration. Bounding only the
	// probe would move the hang rather than remove it: MockOutgoing, StoreMocks,
	// UpdateMockParams and MakeAgentReadyForDockerCompose all go through the
	// same timeout-less AgentClient (pkg/platform/http/agent.go), so a wedged
	// agent would simply block at the first of them instead.
	//
	// Generous because StoreMocks streams the whole corpus, which is the one
	// genuinely large call here; the point is a ceiling, not a tight bound.
	agentRepairTimeout = 2 * time.Minute
)

// probeMockStats reads the agent's stats under mockStatsProbeTimeout.
func (r *Replayer) probeMockStats(ctx context.Context) (models.MockStats, error) {
	probeCtx, cancel := context.WithTimeout(ctx, mockStatsProbeTimeout)
	defer cancel()
	return r.instrumentation.GetMockStats(probeCtx)
}

// agentHoldsStoredCorpus decides whether the agent's loaded-mock count proves
// the corpus stored for this test set is still on the agent the tests would
// fire against. The loaded count is only ever written by StoreMocks on the
// agent process, so any non-zero value means that process received our store;
// zero while a non-empty corpus was stored is the replacement-agent signature
// (an agent the bring-up retry started that was not set up again).
func agentHoldsStoredCorpus(stored, loaded int) bool {
	return stored == 0 || loaded > 0
}

// ensureAgentHoldsStoredMocks verifies, after the docker-compose bring-up and
// before the first test of the set fires, that the agent still holds the mock
// corpus the setup stored on it, and re-registers the session with a
// replacement agent when it does not.
//
// Why this exists: the compose bring-up retry (pkg/client/app,
// shouldRetryComposeUp) answers a transient dependency crash by running `up`
// again, and compose can replace the injected keploy-agent there: it starts
// again an agent it stopped (every service attached), or recreates one (a
// recreate flag), and the replacement boots empty. The CLI's agent session
// sets the replacement up again once it answers (pkg/platform/http/
// session.go); this is the check that it took, and the repair when it did not.
// All of the session's replay state - the stored corpus, the proxy's mock
// manager, the mock filter params - lived in the agent that went away, and
// the straight-line setup above has already run: unrepaired, the tests fire
// against an agent holding nothing and the run reports zero executed tests
// (keploy#4614).
//
// The probe is the agent's non-draining /mock/stats loaded count rather than
// GetConsumedMocks: that one drains, and polling it here would steal entries
// the caller is about to fold into its initial-setup baseline.
//
// On a detected replacement the session is re-registered in the same order the
// setup used (BeforeTestSetCompose -> MockOutgoing -> StoreMocks ->
// SendMockFilterParamsToAgent -> MakeAgentReadyForDockerCompose) and the result
// is confirmed via stats; when the re-registration does not take, the test set
// fails loudly instead of proceeding mockless.
//
// The whole repair runs under agentRepairTimeout, and from the moment a
// replacement is detected every later filter-params send carries the CLI's own
// consumption history (see agentHistoryIncompleteForRun).
//
// agentWatch, under the proven switch, is the set's watch of the agent's
// process (watchAgent). Once the replacement is set up again it keeps to the
// replacement: the app never ran against the agent it replaced (compose's
// retry runs only while the app's container was never started), so none of
// the set's evidence was on that one.
func (r *Replayer) ensureAgentHoldsStoredMocks(ctx context.Context, testRunID, testSetID string, outgoingOpts models.OutgoingOptions, filteredMocks, unfilteredMocks []*models.Mock, totalConsumedMocks map[string]models.MockState, useMappingBased bool, testCases []*models.TestCase, agentWatch models.AgentInstanceWatch) error {
	stored := len(filteredMocks) + len(unfilteredMocks)
	if !r.instrument || stored == 0 {
		return nil
	}

	stats, err := r.probeMockStats(ctx)
	if err != nil {
		// The probe itself failed. A transport blip is not evidence of a
		// replacement, so retry once before concluding anything; if the agent
		// still does not answer, fall through to the re-registration, which
		// fails loudly when the agent is genuinely gone.
		stats, err = r.probeMockStats(ctx)
		if err != nil {
			// ...unless the agent cannot answer this route AT ALL: one that
			// predates /mock/stats, or whose service has no reader. That is
			// version skew, not a replaced agent, and it is unfalsifiable from
			// here — re-registering would "confirm" against the same silence
			// and fail every docker-compose test set on an older agent. Leave
			// the run exactly as it was before this check existed.
			if errors.Is(err, models.ErrMockStatsUnsupported) {
				r.logger.Debug("agent cannot report mock stats; skipping the stored-mocks check for this test set",
					zap.String("testSetID", testSetID), zap.Error(err))
				return nil
			}
			r.logger.Warn("could not verify the agent's stored mocks before firing tests; attempting re-registration",
				zap.String("testSetID", testSetID), zap.Error(err))
		}
	}
	if err == nil && agentHoldsStoredCorpus(stored, stats.Loaded) {
		return nil
	}

	r.logger.Warn("keploy-agent no longer holds this test set's stored mocks (it was replaced during the docker compose bring-up); re-registering the session's mocks with the replacement before any test fires",
		zap.String("testSetID", testSetID), zap.Int("stored", stored), zap.Int("agentLoaded", stats.Loaded))

	// Latch here, at the moment we conclude the agent was replaced, so EVERY
	// later send carries the CLI's consumption map — not just the one below.
	// The agent picks its filter source per request (pkg/service/agent), and a
	// replacement's own history is missing everything consumed before it
	// started, so handing filtering back to it on the next per-test call would
	// re-serve already-consumed mocks for the rest of the run.
	r.agentHistoryIncompleteForRun.Store(true)

	// One ceiling for the whole repair; see agentRepairTimeout.
	ctx, cancelRepair := context.WithTimeout(ctx, agentRepairTimeout)
	defer cancelRepair()

	// Same first step as the setup sequence. The hook rotates the agent's
	// per-test-set debug sink (under KEPLOY_DEBUG_FILE) and runs any non-default
	// AgentHooks; both live in the agent process that just went away. Losing the
	// rotation costs precisely the artifact someone would want when asking why
	// the agent was replaced. Non-fatal here, as it is at the setup call site.
	//
	// r.firstRun is already false by now (setup clears it), which is correct: a
	// repair is not the first run and must not re-trigger first-run cleanup.
	if err := r.hookImpl.BeforeTestSetCompose(ctx, testRunID, testSetID, r.firstRun); err != nil {
		utils.LogError(r.logger, err, "failed to re-run BeforeTestSetCompose hook on the replacement agent")
	}
	// A replacement agent knows no list: it is handed it again.
	if err := realclient.Keep(ctx, r.logger, r.instrumentation, r.mockReal()); err != nil {
		return fmt.Errorf("keploy-agent was replaced during the docker compose bring-up and handing it test set %q's mock.real failed: %w", testSetID, err)
	}
	r.realPushed.Store(true)
	if err := r.instrumentation.MockOutgoing(ctx, outgoingOpts); err != nil {
		return fmt.Errorf("keploy-agent was replaced during the docker compose bring-up and re-registering test set %q's mocks failed at MockOutgoing: %w", testSetID, err)
	}
	if err := r.instrumentation.StoreMocks(ctx, filteredMocks, unfilteredMocks); err != nil {
		return fmt.Errorf("keploy-agent was replaced during the docker compose bring-up and re-registering test set %q's mocks failed at StoreMocks: %w", testSetID, err)
	}
	if err := r.SendMockFilterParamsToAgent(ctx, []string{}, models.BaseTime, time.Now(), r.consumedForAgent(totalConsumedMocks, pkg.PerTestRegion(filteredMocks)), useMappingBased, r.recordedSetShape(ctx, testSetID, testCases)); err != nil {
		return fmt.Errorf("keploy-agent was replaced during the docker compose bring-up and re-registering test set %q's mocks failed at the filter params: %w", testSetID, err)
	}
	// Same soft-fail as the setup call site: a readiness-file write that fails
	// does not stop the run (the compose healthchecks only gate app startup).
	// A proven-switch refusal does stop it, as it does there.
	if err := r.instrumentation.MakeAgentReadyForDockerCompose(ctx); err != nil {
		if errors.Is(err, provenswitch.ErrAgentSwitch) {
			return fmt.Errorf("keploy-agent was replaced during the docker compose bring-up and the new one cannot be run against: %w", err)
		}
		utils.LogError(r.logger, err, "Failed to make the request to make agent ready for the docker compose")
	}
	if agentWatch != nil {
		agentWatch.Accept()
	}

	confirmed, err := r.probeMockStats(ctx)
	if err != nil {
		if errors.Is(err, models.ErrMockStatsUnsupported) {
			// Re-registration ran; this agent simply cannot report the result.
			// The session is no worse off than before the check existed, so do
			// not fail the set on an answer nobody can give.
			r.logger.Debug("agent cannot report mock stats; accepting the re-registration unconfirmed",
				zap.String("testSetID", testSetID))
			return nil
		}
		return fmt.Errorf("keploy-agent was replaced during the docker compose bring-up and re-registering test set %q's mocks could not be confirmed: %w", testSetID, err)
	}
	if !agentHoldsStoredCorpus(stored, confirmed.Loaded) {
		return fmt.Errorf("keploy-agent was replaced during the docker compose bring-up; re-registering test set %q's mocks did not take (the agent still reports no stored mocks), refusing to fire tests against a mockless agent", testSetID)
	}

	r.logger.Info("re-registered the session's mocks with the replacement keploy-agent",
		zap.String("testSetID", testSetID), zap.Int("mocks", stored))
	return nil
}

func (r *Replayer) GetTestSetStatus(ctx context.Context, testRunID string, testSetID string) (models.TestSetStatus, error) {
	testReport, err := r.reportDB.GetReport(ctx, testRunID, testSetID)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to get report: %w", err)
	}
	status, err := models.StringToTestSetStatus(testReport.Status)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to convert string to test set status: %w", err)
	}
	return status, nil
}

func (r *Replayer) CompareHTTPResp(tc *models.TestCase, actualResponse *models.HTTPResp, testSetID string, emitFailureLogs bool) (bool, *models.Result) {
	return r.compareHTTPResp(tc, actualResponse, testSetID, emitFailureLogs)
}

// schemaMatch reports whether a response is compared by its body's schema
// alone (test.schemaMatch, --schema-match). Never under the proven switch,
// where every response body is compared (design v14 U18: the setting is
// deleted at the flip): a schema match passes a value that changed, a field
// that was added and a reorder of same-shaped elements, so a response that
// differs would pass. The setting is then ignored, said once.
func (r *Replayer) schemaMatch() bool {
	if !r.config.Test.SchemaMatch {
		return false
	}
	if provenswitch.On() {
		r.noteIgnored("schemaMatch (--schema-match)", "every response body is compared, value by value")
		return false
	}
	return true
}

// compareHTTPResp is CompareHTTPResp with the matcher's options added.
func (r *Replayer) compareHTTPResp(tc *models.TestCase, actualResponse *models.HTTPResp, testSetID string, emitFailureLogs bool, opts ...httpMatcher.MatchOption) (bool, *models.Result) {
	opts = append([]httpMatcher.MatchOption{r.autoHeaderNoiseOpt()}, opts...)
	noiseConfig := r.httpNoiseConfig(testSetID)
	originalBodySize := originalHTTPRespBodySize(tc, actualResponse)

	if r.schemaMatch() {
		pass, result := httpMatcher.MatchSchema(tc, actualResponse, r.logger, opts...)
		normalizeHTTPRespForReport(tc, actualResponse, originalBodySize)
		return pass, result
	}

	pass, result := httpMatcher.Match(tc, actualResponse, noiseConfig, r.config.Test.IgnoreOrdering, r.config.Test.CompareAll, r.logger, emitFailureLogs, opts...)
	normalizeHTTPRespForReport(tc, actualResponse, originalBodySize)

	return pass, result
}

func (r *Replayer) compareHTTPRespForReplay(tc *models.TestCase, actualResponse *models.HTTPResp, testSetID string, emitFailureLogs bool, opts ...httpMatcher.MatchOption) (bool, *models.Result) {
	opts = append([]httpMatcher.MatchOption{r.autoHeaderNoiseOpt()}, opts...)
	noiseConfig := r.httpNoiseConfig(testSetID)
	originalBodySize := originalHTTPRespBodySize(tc, actualResponse)

	if r.schemaMatch() {
		pass, result := httpMatcher.MatchSchema(tc, actualResponse, r.logger, opts...)
		normalizeHTTPRespForReport(tc, actualResponse, originalBodySize)
		return pass, result
	}

	if emitFailureLogs {
		pass, result := httpMatcher.Match(tc, cloneHTTPResp(actualResponse), noiseConfig, r.config.Test.IgnoreOrdering, r.config.Test.CompareAll, r.logger, false, opts...)
		if pass {
			// The quiet pre-match prints nothing, its pass line included:
			// print it here, as Match does when it emits its logs.
			httpMatcher.PrintPassLine(tc, r.logger, opts...)
			normalizeHTTPRespForReport(tc, actualResponse, originalBodySize)
			return pass, result
		}
		// Skipped outright when the auto-pass is off (auto-replay, the proven
		// switch) rather than left to the helper: this quiet pre-match exists
		// only so the auto-pass gets its shot before failure logs are emitted,
		// and calling the helper here as well as below would log the skip twice
		// for the same test case.
		if !r.additiveAutoPassOff() && r.autoPassHTTPResponseSchemaAddition(tc, actualResponse, testSetID, noiseConfig, result) {
			httpMatcher.PrintPassLine(tc, r.logger, opts...)
			normalizeHTTPRespForReport(tc, actualResponse, originalBodySize)
			return true, result
		}
	}

	pass, result := httpMatcher.Match(tc, actualResponse, noiseConfig, r.config.Test.IgnoreOrdering, r.config.Test.CompareAll, r.logger, emitFailureLogs, opts...)
	normalizeHTTPRespForReport(tc, actualResponse, originalBodySize)
	if !pass && r.autoPassHTTPResponseSchemaAddition(tc, actualResponse, testSetID, noiseConfig, result) {
		return true, result
	}

	return pass, result
}

// autoHeaderNoiseOpt ignores response headers Keploy already treats as
// per-request volatile for egress mock matching — X-Request-Id, Date, W3C and
// Datadog trace ids, cloud signing headers — so a test whose status and body
// match cannot fail on a value the application mints fresh every call.
//
// Honours the existing --disableAutoHeaderNoise flag, which until now only
// covered mock matching. One switch, both places.
func (r *Replayer) autoHeaderNoiseOpt() httpMatcher.MatchOption {
	return httpMatcher.WithAutoHeaderNoise(!r.config.Test.DisableAutoHeaderNoise)
}

func (r *Replayer) httpNoiseConfig(testSetID string) map[string]map[string][]string {
	noiseConfig := deepCopyNoiseConfig(r.config.Test.GlobalNoise.Global)
	if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
		noiseConfig = LeftJoinNoise(noiseConfig, tsNoise)
	}

	return noiseConfig
}

func deepCopyNoiseConfig(src map[string]map[string][]string) map[string]map[string][]string {
	if src == nil {
		return nil
	}

	dst := make(map[string]map[string][]string, len(src))
	for key, inner := range src {
		if inner == nil {
			dst[key] = nil
			continue
		}

		copiedInner := make(map[string][]string, len(inner))
		for innerKey, values := range inner {
			if values == nil {
				copiedInner[innerKey] = nil
				continue
			}

			copiedInner[innerKey] = append([]string(nil), values...)
		}

		dst[key] = copiedInner
	}

	return dst
}

func cloneHTTPResp(resp *models.HTTPResp) *models.HTTPResp {
	if resp == nil {
		return nil
	}

	clone := *resp
	if resp.Header != nil {
		clone.Header = make(map[string]string, len(resp.Header))
		for key, value := range resp.Header {
			clone.Header[key] = value
		}
	}

	return &clone
}

func originalHTTPRespBodySize(tc *models.TestCase, actualResponse *models.HTTPResp) int64 {
	if tc == nil || actualResponse == nil || !tc.HTTPResp.BodySkipped {
		return 0
	}

	return int64(len(actualResponse.Body))
}

func normalizeHTTPRespForReport(tc *models.TestCase, actualResponse *models.HTTPResp, originalBodySize int64) {
	if tc == nil || actualResponse == nil || !tc.HTTPResp.BodySkipped {
		return
	}

	actualResponse.BodySize = originalBodySize
	actualResponse.BodySkipped = true
	actualResponse.Body = ""
}

func (r *Replayer) autoPassHTTPResponseSchemaAddition(tc *models.TestCase, actualResponse *models.HTTPResp, testSetID string, noiseConfig map[string]map[string][]string, result *models.Result) bool {
	// Auto-replay never auto-passes an additive change. The pass is for a NEWER
	// build that legitimately grew a response field; auto-replay runs the binary
	// it just recorded, so it cannot have grown one. An addition here is
	// nondeterminism or a mock served from the wrong window, and waving it
	// through hides exactly the class of defect auto-replay exists to catch. Let
	// it fall through to the normal verdict so the caller grades it instead.
	if r.config.Test.AutoReplay {
		if qualifiesForHTTPResponseSchemaAdditionPass(result) {
			r.logger.Info("skipping additive response-schema auto-pass during auto-replay",
				zap.String("testcase", tc.Name),
				zap.String("testset", testSetID))
		}
		return false
	}
	// The proven model asserts every response field: a response that gained a
	// field is a difference like any other, and the test fails on it.
	if provenswitch.On() {
		if qualifiesForHTTPResponseSchemaAdditionPass(result) {
			r.logger.Info("not auto-passing an additive response-schema change: every response field is asserted (KEPLOY_PROVEN=1)",
				zap.String("testcase", tc.Name),
				zap.String("testset", testSetID))
		}
		return false
	}
	if !qualifiesForHTTPResponseSchemaAdditionPass(result) {
		return false
	}

	assessment, assessmentErr := matcherUtils.ComputeFailureAssessmentJSON(
		tc.HTTPResp.Body,
		actualResponse.Body,
		bodyNoiseForTestCase(tc.Noise, noiseConfig),
		r.config.Test.IgnoreOrdering,
	)

	fields := []zap.Field{
		zap.String("protocol", "http"),
		zap.String("testcase", tc.Name),
		zap.String("testset", testSetID),
	}
	if assessment != nil {
		if len(assessment.AddedFields) > 0 {
			fields = append(fields, zap.Strings("added_fields", assessment.AddedFields))
		}
		if len(assessment.Reasons) > 0 {
			fields = append(fields, zap.Strings("assessment_reasons", assessment.Reasons))
		}
	}
	if assessmentErr != nil {
		fields = append(fields, zap.NamedError("assessment_error", assessmentErr))
	}

	r.logger.Info(
		"Additive response schema change detected. Passing testcase by default because only new response fields were added. Verify downstream consumers are not affected by the schema expansion.",
		fields...,
	)

	normalizePassingHTTPResult(result)
	return true
}

func qualifiesForHTTPResponseSchemaAdditionPass(result *models.Result) bool {
	if result == nil {
		return false
	}

	// Risk is the only signal that separates "the response gained fields" from
	// "the response gained fields AND an existing value moved". AssessJSON grades
	// the first Low and the second Medium, both under category SchemaAdded — so
	// the category set alone cannot tell them apart and the Risk gate has to
	// apply to every shape below, not just the first.
	//
	// It previously guarded only the plain SchemaAdded branch. The
	// Content-Length branch ran unguarded, and since adding a field to a JSON
	// body always changes Content-Length, that branch answered first for
	// essentially every additive diff — which made the Low gate unreachable and
	// auto-passed Medium-risk value changes. See #4578.
	// Read the BODY's risk, not FailureInfo.Risk. The latter is the max across
	// status/header/body (pkg/matcher/http/match.go), and HeaderChanged is only
	// ever appended inside a block that unconditionally maxes in Medium — so a
	// Content-Length diff alone forces the aggregate to Medium and would reject
	// the very case this pass exists for. FailureInfo.Assessment holds the
	// AssessJSON grade verbatim: Low = only new fields, Medium = new fields plus
	// value changes on existing fields.
	assessment := result.FailureInfo.Assessment
	if assessment == nil || assessment.Risk != models.Low {
		return false
	}

	return hasOnlyFailureCategories(result.FailureInfo.Category, models.SchemaAdded) ||
		hasOnlySchemaAdditionAndContentLengthDiff(result)
}

func hasOnlyFailureCategories(categories []models.FailureCategory, allowed ...models.FailureCategory) bool {
	if len(categories) == 0 {
		return false
	}

	allowedSet := make(map[models.FailureCategory]struct{}, len(allowed))
	for _, category := range allowed {
		allowedSet[category] = struct{}{}
	}

	for _, category := range categories {
		if _, ok := allowedSet[category]; !ok {
			return false
		}
	}

	return true
}

func hasOnlySchemaAdditionAndContentLengthDiff(result *models.Result) bool {
	if !hasOnlyFailureCategories(result.FailureInfo.Category, models.SchemaAdded, models.HeaderChanged) {
		return false
	}

	hasSchemaAdded := false
	hasHeaderChanged := false
	for _, category := range result.FailureInfo.Category {
		hasSchemaAdded = hasSchemaAdded || category == models.SchemaAdded
		hasHeaderChanged = hasHeaderChanged || category == models.HeaderChanged
	}

	return hasSchemaAdded && hasHeaderChanged && hasOnlyContentLengthDiff(result.HeadersResult)
}

func hasOnlyContentLengthDiff(headers []models.HeaderResult) bool {
	foundDiff := false

	for _, header := range headers {
		if header.Normal {
			continue
		}

		foundDiff = true
		key := strings.ToLower(header.Expected.Key)
		if key == "" {
			key = strings.ToLower(header.Actual.Key)
		}
		if key != "content-length" {
			return false
		}
		if len(header.Expected.Value) == 0 || len(header.Actual.Value) == 0 {
			return false
		}
	}

	return foundDiff
}

func normalizePassingHTTPResult(result *models.Result) {
	if result == nil {
		return
	}

	result.FailureInfo = models.FailureInfo{}
	result.StatusCode.Normal = true
	result.BodySizeResult.Normal = true

	for i := range result.HeadersResult {
		result.HeadersResult[i].Normal = true
	}
	for i := range result.BodyResult {
		result.BodyResult[i].Normal = true
	}
	for i := range result.TrailerResult {
		result.TrailerResult[i].Normal = true
	}
}

func bodyNoiseForTestCase(testCaseNoise map[string][]string, noiseConfig map[string]map[string][]string) map[string][]string {
	bodyNoise := matcherUtils.CloneNoiseMap(noiseConfig["body"])

	// nil logger on purpose: Match already ran SplitNoise for this same test
	// case and emitted any warning, so passing one here would only duplicate it.
	tcBodyNoise, _, _ := matcherUtils.SplitNoise(testCaseNoise, nil)
	for field, regexArr := range tcBodyNoise {
		// SplitNoise already returns freshly-allocated slices.
		bodyNoise[field] = regexArr
	}

	return bodyNoise
}

func (r *Replayer) CompareGRPCResp(tc *models.TestCase, actualResp *models.GrpcResp, testSetID string, emitFailureLogs bool) (bool, *models.Result) {
	return r.compareGRPCResp(tc, actualResp, testSetID, emitFailureLogs, false)
}

// compareGRPCResp is CompareGRPCResp for a test case that may already have
// failed on its own (failedElsewhere: a template variable's place): a
// comparison that matches then prints no pass line for it.
func (r *Replayer) compareGRPCResp(tc *models.TestCase, actualResp *models.GrpcResp, testSetID string, emitFailureLogs, failedElsewhere bool) (bool, *models.Result) {
	noiseConfig := r.config.Test.GlobalNoise.Global
	if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
		noiseConfig = LeftJoinNoise(r.config.Test.GlobalNoise.Global, tsNoise)
	}

	var opts []grpcMatcher.MatchOption
	if failedElsewhere {
		opts = append(opts, grpcMatcher.WithoutPassLine())
	}
	return grpcMatcher.Match(tc, actualResp, noiseConfig, r.config.Test.IgnoreOrdering, r.logger, emitFailureLogs, opts...)
}

func (r *Replayer) printSummary(_ context.Context, _ bool) {
	if r.config.JSONOutput {
		return
	}

	r.completeTestReportMu.RLock()
	totalTestsSnapshot := r.totalTests
	totalTestPassedSnapshot := r.totalTestPassed
	totalTestFailedSnapshot := r.totalTestFailed
	totalTestObsoleteSnapshot := r.totalTestObsolete
	totalTestIgnoredSnapshot := r.totalTestIgnored
	totalTestTimeTakenSnapshot := r.totalTestTimeTaken
	reportSnapshot := make(map[string]TestReportVerdict, len(r.completeTestReport))
	for key, val := range r.completeTestReport {
		reportSnapshot[key] = val
	}
	r.completeTestReportMu.RUnlock()

	if totalTestsSnapshot > 0 {
		testSuiteNames := make([]string, 0, len(reportSnapshot))
		for testSuiteName := range reportSnapshot {
			testSuiteNames = append(testSuiteNames, testSuiteName)
		}
		sort.SliceStable(testSuiteNames, func(i, j int) bool {
			testSuitePartsI := strings.Split(testSuiteNames[i], "-")
			testSuitePartsJ := strings.Split(testSuiteNames[j], "-")
			if len(testSuitePartsI) < 3 || len(testSuitePartsJ) < 3 {
				return testSuiteNames[i] < testSuiteNames[j]
			}
			testSuiteIDNumberI, err1 := strconv.Atoi(testSuitePartsI[2])
			testSuiteIDNumberJ, err2 := strconv.Atoi(testSuitePartsJ[2])
			if err1 != nil || err2 != nil {
				return false
			}
			return testSuiteIDNumberI < testSuiteIDNumberJ
		})

		totalTestTimeTakenStr := timeWithUnits(totalTestTimeTakenSnapshot)

		if !r.config.DisableANSI {
			summaryFormat := "\n <=========================================> \n  COMPLETE TESTRUN SUMMARY. \n" +
				"\tTotal tests: %s\n" +
				"\tTotal test passed: %s\n" +
				"\tTotal test failed: %s\n"
			summaryArgs := []interface{}{totalTestsSnapshot, totalTestPassedSnapshot, totalTestFailedSnapshot}
			if totalTestObsoleteSnapshot > 0 {
				summaryFormat += "\tTotal test obsolete: %s\n"
				summaryArgs = append(summaryArgs, totalTestObsoleteSnapshot)
			}
			if totalTestIgnoredSnapshot > 0 {
				summaryFormat += "\tTotal test ignored: %s\n"
				summaryArgs = append(summaryArgs, totalTestIgnoredSnapshot)
			}
			summaryFormat += "\tTotal time taken: %s\n"
			summaryArgs = append(summaryArgs, totalTestTimeTakenStr)

			if _, err := pp.Printf(summaryFormat, summaryArgs...); err != nil {
				utils.LogError(r.logger, err, "failed to print test run summary")
				return
			}

			header := "\n\tTest Suite Name\t\tTotal Test\tPassed\t\tFailed"
			if totalTestObsoleteSnapshot > 0 {
				header += "\t\tObsolete"
			}
			if totalTestIgnoredSnapshot > 0 {
				header += "\t\tIgnored"
			}
			header += "\t\tTime Taken"
			if totalTestFailedSnapshot > 0 {
				header += "\tFailed Testcases"
			}
			header += "\t\n"

			_, err := pp.Printf(header)
			if err != nil {
				utils.LogError(r.logger, err, "failed to print test suite summary header")
				return
			}

			for _, testSuiteName := range testSuiteNames {
				report := reportSnapshot[testSuiteName]
				if report.status {
					pp.SetColorScheme(models.GetPassingColorScheme())
				} else {
					pp.SetColorScheme(models.GetFailingColorScheme())
				}

				testSetTimeTakenStr := timeWithUnits(report.duration)

				var format strings.Builder
				args := []interface{}{}

				format.WriteString("\n\t%s\t\t%s\t\t%s\t\t%s")
				args = append(args, testSuiteName, report.total, report.passed, report.failed)

				if totalTestObsoleteSnapshot > 0 {
					format.WriteString("\t\t%s")
					args = append(args, report.obsolete)
				}

				if totalTestIgnoredSnapshot > 0 && !r.config.Test.MustPass {
					format.WriteString("\t\t%s")
					args = append(args, report.ignored)
				}

				format.WriteString("\t\t%s") // Time Taken
				args = append(args, testSetTimeTakenStr)

				if totalTestFailedSnapshot > 0 && !r.config.Test.MustPass {
					failedCasesStr := "-"
					if failedCases, ok := r.failedTCsBySetID[testSuiteName]; ok && len(failedCases) > 0 {
						failedCasesStr = strings.Join(failedCases, ", ")
					}
					format.WriteString("\t%s")
					args = append(args, failedCasesStr)
				}

				if _, err := pp.Printf(format.String(), args...); err != nil {
					utils.LogError(r.logger, err, "failed to print test suite details")
					return
				}
			}
			fmt.Print(r.quarantinedSummary())
			if _, err := pp.Printf("\n<=========================================> \n\n"); err != nil {
				utils.LogError(r.logger, err, "failed to print separator")
				return
			}

		} else {
			fmt.Printf("\n <=========================================> \n  COMPLETE TESTRUN SUMMARY. \n\tTotal tests: %d\n"+"\tTotal test passed: %d\n"+"\tTotal test failed: %d\n", totalTestsSnapshot, totalTestPassedSnapshot, totalTestFailedSnapshot)
			if totalTestObsoleteSnapshot > 0 {
				fmt.Printf("\tTotal test obsolete: %d\n", totalTestObsoleteSnapshot)
			}
			if totalTestIgnoredSnapshot > 0 {
				fmt.Printf("\tTotal test ignored: %d\n", totalTestIgnoredSnapshot)
			}
			fmt.Printf("\tTotal time taken: %s\n", totalTestTimeTakenStr)

			header := "\n\tTest Suite Name\t\tTotal Test\tPassed\t\tFailed"
			if totalTestObsoleteSnapshot > 0 {
				header += "\t\tObsolete"
			}
			if totalTestIgnoredSnapshot > 0 {
				header += "\t\tIgnored"
			}
			header += "\t\tTime Taken"
			if totalTestFailedSnapshot > 0 {
				header += "\tFailed Testcases"
			}
			header += "\t\n"

			fmt.Print(header)

			for _, testSuiteName := range testSuiteNames {
				report := reportSnapshot[testSuiteName]
				testSetTimeTakenStr := timeWithUnits(report.duration)

				var format strings.Builder
				args := []interface{}{}

				format.WriteString("\n\t%s\t\t%d\t\t%d\t\t%d")
				args = append(args, testSuiteName, report.total, report.passed, report.failed)

				if totalTestObsoleteSnapshot > 0 {
					format.WriteString("\t\t%d")
					args = append(args, report.obsolete)
				}

				if totalTestIgnoredSnapshot > 0 && !r.config.Test.MustPass {
					format.WriteString("\t\t%d")
					args = append(args, report.ignored)
				}

				format.WriteString("\t\t%s") // Time Taken
				args = append(args, testSetTimeTakenStr)

				if totalTestFailedSnapshot > 0 && !r.config.Test.MustPass {
					failedCasesStr := "-"
					if failedCases, ok := r.failedTCsBySetID[testSuiteName]; ok && len(failedCases) > 0 {
						failedCasesStr = strings.Join(failedCases, ", ")
					}
					format.WriteString("\t\t%s")
					args = append(args, failedCasesStr)
				}
				fmt.Printf(format.String(), args...)
			}

			fmt.Print(r.quarantinedSummary())
			fmt.Print("\n<=========================================> \n\n")
		}
	}
}
func (r *Replayer) RunApplication(ctx context.Context, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, opts)
}

func (r *Replayer) GetTestSetConf(ctx context.Context, testSet string) (*models.TestSet, error) {
	return r.testSetConf.Read(ctx, testSet)
}

func (r *Replayer) DenoiseTestCases(ctx context.Context, testSetID string, noiseParams []*models.NoiseParams) ([]*models.NoiseParams, error) {

	testCases, err := r.testDB.GetTestCases(ctx, testSetID)
	if err != nil {
		return nil, fmt.Errorf("failed to get test cases: %w", err)
	}

	for _, v := range testCases {
		for _, noiseParam := range noiseParams {
			if v.Name == noiseParam.TestCaseID {
				// append the noise map
				if noiseParam.Ops == string(models.OpsAdd) {
					v.Noise = mergeMaps(v.Noise, noiseParam.Assertion)
				} else {
					// remove from the original noise map
					v.Noise = removeFromMap(v.Noise, noiseParam.Assertion)
				}
				err = r.testDB.UpdateTestCase(ctx, v, testSetID, true)
				if err != nil {
					return nil, fmt.Errorf("failed to update test case: %w", err)
				}
				noiseParam.AfterNoise = v.Noise
			}
		}
	}

	return noiseParams, nil
}

func (r *Replayer) executeScript(ctx context.Context, script string) error {

	if script == "" {
		return nil
	}

	// Define the function to cancel the command
	cmdCancel := func(cmd *exec.Cmd) func() error {
		return func() error {
			return utils.InterruptProcessTree(r.logger, cmd.Process.Pid, syscall.SIGINT)
		}
	}

	cmdErr := utils.ExecuteCommand(ctx, r.logger, script, utils.Empty, cmdCancel, 25*time.Second, nil, nil)
	if cmdErr.Err != nil {
		return fmt.Errorf("failed to execute script: %w", cmdErr.Err)
	}
	return nil
}

func (r *Replayer) DeleteTestSet(ctx context.Context, testSetID string) error {
	return r.testDB.DeleteTestSet(ctx, testSetID)
}

func (r *Replayer) DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error {
	return r.testDB.DeleteTests(ctx, testSetID, testCaseIDs)
}

// appendCategoryUnique appends c only if it is not already present.
func appendCategoryUnique(cats []models.FailureCategory, c models.FailureCategory) []models.FailureCategory {
	for _, x := range cats {
		if x == c {
			return cats
		}
	}
	return append(cats, c)
}

// anyAppConnectionError reports whether any test in the run failed because the
// application produced no response at all (connection refused/reset/EOF). Such a
// test says nothing about which mocks are needed — its request never reached the
// app — so it must not be read as evidence that mocks are unused.
//
// Reads the TOP-LEVEL TestResult.FailureInfo, not Result.FailureInfo, for the
// simple reason that CreateFailedTestResult only sets the category there. Both
// survive the report store (it holds the structs in memory; ReportResults reads
// Result.FailureInfo.Risk off the very same values), so this is about where the
// category is written, not about what survives.
func anyAppConnectionError(results []models.TestResult) bool {
	for _, tr := range results {
		for _, c := range tr.FailureInfo.Category {
			if c == models.AppConnectionError {
				return true
			}
		}
	}
	return false
}

// pruneInputUntrustworthy reports whether a run's consumed-mock set is too weak
// to justify deleting anything.
//
// Pruning keeps what the tests consumed and deletes the rest, which is only sound
// when a test not consuming a mock actually means "the app didn't need it". Two
// cases break that:
//
//   - success == 0: no test passed, so nothing vouches for any mock. The keep-set
//     collapses to what the startup/never-executed paths contributed, and every
//     mock a real request would have used is deleted.
//   - appUnreachable (any AppConnectionError, see anyAppConnectionError): those
//     requests never reached the app, so they consumed no mocks for want of a
//     connection, not for want of need.
//
// Both mean "no information", and pruning reads that as "delete" — which is the
// bug. Note this is NOT the same as "the run produced no signal at all": a run
// can have passing tests AND one connection error, and it lands here because that
// one test's mocks would be wrongly deleted. Callers must not describe it as a
// zero-passing run.
func pruneInputUntrustworthy(success int, appUnreachable bool) bool {
	return success == 0 || appUnreachable
}

// shouldSkipPruning decides whether a completed run is allowed to delete recorded
// mocks. Pruning is destructive and its input (the keep-set) is only trustworthy
// when the run actually produced signal, so there are two independent reasons to
// refuse:
//
//   - the keep-set is untrustworthy (pruneInputUntrustworthy): no test passed, or
//     some request never reached the app. Mocks went unconsumed for lack of
//     evidence, not because they are unused. This reason is deliberately
//     INDEPENDENT of preserveFailedMocks: callers turn that flag off (k8s-proxy
//     auto-replay sets it false), so it cannot serve as the safety net here. A
//     crashed container must never be able to destroy mocks.
//   - preserveFailedMocks (opt-in): a caller that wants every recorded mock kept
//     available for inspection whenever anything failed or went obsolete.
//
// shouldPrune is the COMPLETE gate on the destructive prune: it must be enabled
// (RemoveUnusedMocks), keploy must be instrumenting the run, and the run's
// consumed-mock set must be trustworthy enough to delete against. When it
// refuses, the second value names the rule, for the "skipping mock pruning" log:
//
//   - capture_incomplete: the agent could not track a process the application
//     ran (pidmaps.go). Its calls reached the real dependency, so the mocks
//     recorded for them were consumed by nobody, and every test may have passed.
//     Under the proven switch, also a call keploy lost while a test ran, or
//     after the set's last test (a TLS handshake a client aborted, a parser
//     stall): the mock recorded for it was consumed by nobody either.
//   - not_every_test_got_a_verdict: a subset or stopped run. The tests it did
//     not score consumed nothing, and only mappings.yaml protects their mocks.
//   - mapping_stale: a test answered wrong while its calls diverged from its
//     recorded mapping (FailureInfo.MappingStale). What it consumed is no
//     verdict's, and the mapped mock it left unconsumed may be what a
//     re-record shows it still needs.
//   - internal_error: keploy lost part of the run, such as a result insert.
//   - results_unreadable: the results the rules below read are missing or short.
//   - no_answer: a request consumed nothing for want of an answer.
//   - capture_unverified: the agent counts untracked processes, but the count
//     could not be read, so nothing shows the consumed-mock set is complete.
//     It is no evidence of a loss, so it comes after the rules that name what
//     did go wrong: an agent stopped mid-run loses the count too, and the
//     stop (not_every_test_got_a_verdict) is what the line must say.
//   - app_unreachable, no_test_passed, preserve_failed_mocks: shouldSkipPruning.
//
// It exists as one function because the data-loss bug lives in this conjunction,
// not in shouldSkipPruning alone — a correct predicate that isn't wired into the
// decision deletes mocks just the same. Keeping the whole condition here means a
// unit test can pin the wiring instead of only the predicate. A zero pruneRun
// never prunes.
func shouldPrune(removeUnusedMocks, instrument bool, run pruneRun) (bool, string) {
	switch {
	case !removeUnusedMocks || !instrument:
		return false, "disabled"
	case run.captureIncomplete:
		return false, "capture_incomplete"
	case run.unchecked:
		return false, "unchecked"
	case !run.allTestsGotAVerdict:
		return false, "not_every_test_got_a_verdict"
	case run.mappingStale:
		return false, "mapping_stale"
	case run.internalErr:
		return false, "internal_error"
	case !run.resultsComplete:
		return false, "results_unreadable"
	case run.noAnswer:
		return false, "no_answer"
	case run.captureUnverified:
		return false, "capture_unverified"
	case !shouldSkipPruning(run.success, run.failure, run.obsolete, run.preserveFailedMocks, run.appUnreachable):
		return true, ""
	case run.appUnreachable:
		return false, "app_unreachable"
	case run.success == 0:
		return false, "no_test_passed"
	}
	return false, "preserve_failed_mocks"
}

// pruneRun is what a finished run tells shouldPrune.
type pruneRun struct {
	allTestsGotAVerdict        bool // every loaded, non-ignored test was scored
	internalErr                bool // the set ended INTERNAL_ERR
	resultsComplete            bool // results read back, none short of the verdicts
	noAnswer                   bool // a request got no answer (pkg.IsAppNoAnswer)
	appUnreachable             bool // anyAppConnectionError over the results read back
	captureIncomplete          bool // the agent could not track a process (pidMapCaptureIncomplete), or lost a call (proven switch)
	captureUnverified          bool // the agent counts, but its count could not be read (pidMapVerdict)
	unchecked                  bool // a hook could not read all the set's tests read (UncheckedHook)
	mappingStale               bool // a test is not judged for a stale mapping (FailureInfo.MappingStale)
	success, failure, obsolete int
	preserveFailedMocks        bool
}

// shouldSkipPruning takes appUnreachable as computed once per set by
// anyAppConnectionError, so the prune and the mappings hold read one value.
func shouldSkipPruning(success, failure, obsolete int, preserveFailedMocks, appUnreachable bool) bool {
	if pruneInputUntrustworthy(success, appUnreachable) {
		return true
	}
	return preserveFailedMocks && (failure > 0 || obsolete > 0)
}

// CreateFailedTestResult builds the result of a test case that failed without
// a response to compare: err is why, and its text stands in for the response.
func (r *Replayer) CreateFailedTestResult(testCase *models.TestCase, testSetID string, started time.Time, err error) *models.TestResult {
	errorMessage := err.Error()
	testCaseResult := &models.TestResult{
		Kind:         testCase.Kind,
		Name:         testSetID,
		Status:       models.TestStatusFailed,
		Started:      started.Unix(),
		Completed:    time.Now().UTC().Unix(),
		TestCaseID:   testCase.Name,
		TestCasePath: filepath.Join(r.config.Path, testSetID),
		MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
		Noise:        testCase.Noise,
		TimeTaken:    time.Since(started).String(),
	}

	var result *models.Result

	switch testCase.Kind {
	case models.HTTP:
		actualResponse := &models.HTTPResp{
			StatusCode: 0,
			Header:     make(map[string]string),
			Body:       errorMessage,
		}

		// The test case has failed on its own (no answer): what its
		// assertions say of the stand-in is printed, never a pass line.
		_, result = r.compareHTTPResp(testCase, actualResponse, testSetID, true, httpMatcher.WithoutPassLine())

		testCaseResult.Req = models.HTTPReq{
			Method:     testCase.HTTPReq.Method,
			ProtoMajor: testCase.HTTPReq.ProtoMajor,
			ProtoMinor: testCase.HTTPReq.ProtoMinor,
			URL:        testCase.HTTPReq.URL,
			URLParams:  testCase.HTTPReq.URLParams,
			Header:     testCase.HTTPReq.Header,
			Body:       testCase.HTTPReq.Body,
			Binary:     testCase.HTTPReq.Binary,
			Form:       testCase.HTTPReq.Form,
			Timestamp:  testCase.HTTPReq.Timestamp,

			HeaderLineLengths: testCase.HTTPReq.HeaderLineLengths,
		}
		testCaseResult.Res = *actualResponse

	case models.GRPC_EXPORT:
		actualResponse := &models.GrpcResp{
			Headers: models.GrpcHeaders{
				PseudoHeaders:   make(map[string]string),
				OrdinaryHeaders: make(map[string]string),
			},
			Body: models.GrpcLengthPrefixedMessage{
				DecodedData: errorMessage,
			},
			Trailers: models.GrpcHeaders{
				PseudoHeaders:   make(map[string]string),
				OrdinaryHeaders: make(map[string]string),
			},
		}

		respCopy := *actualResponse

		if r.config.Test.ProtoFile != "" || r.config.Test.ProtoDir != "" || len(r.config.Test.ProtoInclude) > 0 {
			// get the :path header from the request
			method, ok := testCase.GrpcReq.Headers.PseudoHeaders[":path"]
			if !ok {
				utils.LogError(r.logger, nil, "failed to get :path header from the request, cannot convert grpc response to json")
				goto compareResp
			}

			pc := models.ProtoConfig{
				ProtoFile:    r.config.Test.ProtoFile,
				ProtoDir:     r.config.Test.ProtoDir,
				ProtoInclude: r.config.Test.ProtoInclude,
				RequestURI:   method,
			}

			// get the proto message descriptor
			md, files, err := utils.GetProtoMessageDescriptor(context.Background(), r.logger, pc)
			if err != nil {
				utils.LogError(r.logger, err, "failed to get proto message descriptor, cannot convert grpc response to json")
				goto compareResp
			}

			// convert both actual and expected using the same path (protoscope-text -> wire -> json)
			actJSON, actOK := utils.ProtoTextToJSON(md, files, respCopy.Body.DecodedData, r.logger)
			testJSON, testOK := utils.ProtoTextToJSON(md, files, testCase.GrpcResp.Body.DecodedData, r.logger)

			if actOK && testOK {
				respCopy.Body.DecodedData = string(actJSON)
				testCase.GrpcResp.Body.DecodedData = string(testJSON)
			}
		}

	compareResp:
		_, result = r.CompareGRPCResp(testCase, &respCopy, testSetID, true)

		testCaseResult.GrpcReq = testCase.GrpcReq
		testCaseResult.GrpcRes = *actualResponse
	}

	if result != nil {
		testCaseResult.Result = *result
	}

	if result != nil && result.FailureInfo.Risk != models.None {
		testCaseResult.FailureInfo.Risk = result.FailureInfo.Risk
		testCaseResult.FailureInfo.Category = result.FailureInfo.Category
	}

	// Attribute a connection-level failure distinctly: the status_code=0 recorded
	// above is the synthetic value we use when the app produced NO response. If the
	// cause is a transport error (pkg.IsAppConnectionError: refused, dropped, host
	// not found) it is an app-unreachable/availability failure, NOT a content
	// regression — label it so operators and downstream (k8s-proxy reads
	// TestResult.FailureInfo) triage it as infra rather than a STATUS_CODE_CHANGED
	// regression. Raw StatusCode stays 0. The error is classified, not its text:
	// the reset re-send and the unreachable-port check read the same error.
	if pkg.IsAppConnectionError(err) {
		testCaseResult.FailureInfo.Category = appendCategoryUnique(testCaseResult.FailureInfo.Category, models.AppConnectionError)
	}

	return testCaseResult
}

func (r *Replayer) replaceHostInTestCase(testCase *models.TestCase, newHost, logContext string) error {
	var err error
	switch testCase.Kind {
	case models.HTTP:
		testCase.HTTPReq.URL, err = utils.ReplaceHost(testCase.HTTPReq.URL, newHost)
		if err != nil {
			utils.LogError(r.logger, err, fmt.Sprintf("failed to replace host to %s", logContext))
			return err
		}
		r.logger.Debug("", zap.String(fmt.Sprintf("replaced %s", logContext), testCase.HTTPReq.URL))

	case models.GRPC_EXPORT:
		testCase.GrpcReq.Headers.PseudoHeaders[":authority"], err = utils.ReplaceGrpcHost(testCase.GrpcReq.Headers.PseudoHeaders[":authority"], newHost)
		if err != nil {
			utils.LogError(r.logger, err, fmt.Sprintf("failed to replace host to %s", logContext))
			return err
		}
		r.logger.Debug("", zap.String(fmt.Sprintf("replaced %s", logContext), testCase.GrpcReq.Headers.PseudoHeaders[":authority"]))
	}
	return nil
}

func (r *Replayer) replacePortInTestCase(testCase *models.TestCase, newPort string) error {
	var err error
	switch testCase.Kind {
	case models.HTTP:
		testCase.HTTPReq.URL, err = utils.ReplacePort(testCase.HTTPReq.URL, newPort)
	case models.GRPC_EXPORT:
		testCase.GrpcReq.Headers.PseudoHeaders[":authority"], err = utils.ReplaceGrpcPort(testCase.GrpcReq.Headers.PseudoHeaders[":authority"], newPort)
	}
	if err != nil {
		utils.LogError(r.logger, err, "failed to replace port")
		return err
	}
	return nil
}

func (r *Replayer) GetSelectedTestSets(ctx context.Context) ([]string, error) {
	// get all the testset ids
	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		utils.LogError(r.logger, err, "failed to get all test set ids")
		return nil, fmt.Errorf("failed to get test set ids")
	}
	return r.selectTestSets(testSetIDs)
}

// selectTestSets is the recorded test sets (testSetIDs) a run replays: those
// test.selectedTests names (-t/--test-sets, or keploy.yml), or every one when
// it names none, in their natural order and then the configured one.
//
// A name that is no recorded test set was dropped silently: -t
// "test-set-1,test-set-3" replayed test-set-1 alone and passed, and a
// selection naming no recorded set at all replayed every set. Now it is
// refused, naming it, under the proven switch and, with the switch off,
// whenever the selection is the command line's own
// (Test.SelectedFromCommandLine). With the switch off, a name keploy.yml's
// test.selectedTests inherited is still left out, with a Warn naming it, so a
// stale keploy.yml does not turn a run that passed red on upgrade; when it
// leaves no name, every set is replayed, as before.
//
// keploy.yml's test.selectedTests arrives with its keys lowercased (viper
// reads every key so: {MySet: []} is {myset: []}), so a name that is no
// recorded set's, but one recorded set's alone in another case, names that
// set, and the selection is keyed by it again, as the replay reads it per set.
func (r *Replayer) selectTestSets(testSetIDs []string) ([]string, error) {
	recorded := make(map[string]bool, len(testSetIDs))
	folded := make(map[string][]string, len(testSetIDs))
	for _, id := range testSetIDs {
		recorded[id] = true
		folded[strings.ToLower(id)] = append(folded[strings.ToLower(id)], id)
	}
	selected := r.config.Test.SelectedTests
	keyed := make(map[string][]string, len(selected))
	var unknown []string
	for name, cases := range selected {
		if recorded[name] {
			keyed[name] = cases
		}
	}
	for name, cases := range selected {
		if recorded[name] {
			continue
		}
		ids := folded[strings.ToLower(name)]
		if len(ids) != 1 {
			unknown = append(unknown, name)
			continue
		}
		if _, named := keyed[ids[0]]; !named {
			keyed[ids[0]] = cases
		}
	}
	if len(unknown) > 0 {
		natsort.Sort(unknown)
		known := slices.Clone(testSetIDs)
		natsort.Sort(known)
		if provenswitch.On() || r.config.Test.SelectedFromCommandLine {
			err := fmt.Errorf("the test set(s) %s named for this run are not recorded", strings.Join(unknown, ", "))
			utils.LogError(r.logger, err, "the run names a test set that is not recorded, so it replays none",
				zap.Strings("notRecorded", unknown),
				zap.Strings("recorded", known),
				zap.String("next_step", "name only recorded test sets in -t/--test-sets (comma-separated) or in test.selectedTests in keploy.yml, or record the missing ones with keploy record"))
			return nil, err
		}
		left := "the rest of the selection is replayed"
		if len(keyed) == 0 {
			left = "it names no recorded set, so every recorded set is replayed, as with no selection"
		}
		r.logger.Warn("keploy.yml's test.selectedTests names a test set that is not recorded, so this run leaves it out: "+left,
			zap.Strings("notRecorded", unknown),
			zap.Strings("recorded", known),
			zap.String("next_step", "remove the set from test.selectedTests in keploy.yml, or record it with keploy record; named with -t, or with the proven switch on, a set that is not recorded fails the run"))
	}
	if len(selected) > 0 {
		r.config.Test.SelectedTests = keyed
	}
	var testSets []string
	for _, id := range testSetIDs {
		if _, ok := r.config.Test.SelectedTests[id]; ok || len(r.config.Test.SelectedTests) == 0 {
			testSets = append(testSets, id)
		}
	}
	natsort.Sort(testSets)
	return r.applyTestSetOrder(testSets), nil
}

func (r *Replayer) applyTestSetOrder(testSets []string) []string {
	orderer, ok := r.hookImpl.(TestSetOrderer)
	if !ok {
		return testSets
	}
	reordered := orderer.OrderTestSets(testSets)
	if len(reordered) != len(testSets) {
		return testSets
	}
	return reordered
}

func (r *Replayer) StoreMappings(ctx context.Context, mapping *models.Mapping) error {
	// Save test-mock mappings to YAML file
	err := r.mappingDB.Insert(ctx, mapping)
	return err
}

// createBackup creates a timestamped backup of a test set directory before modification.
func (r *Replayer) createBackup(testSetID string) error {
	srcPath := filepath.Join(r.config.Path, testSetID)
	timestamp := time.Now().Format("20060102T150405")
	backupDestPath := filepath.Join(srcPath, ".backup", timestamp)

	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return fmt.Errorf("source directory for backup does not exist: %s", srcPath)
	}

	if err := os.MkdirAll(backupDestPath, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	err := r.copyDirContents(srcPath, backupDestPath)
	if err != nil {
		// Clean up the failed backup attempt
		_ = os.RemoveAll(backupDestPath)
		return fmt.Errorf("failed to copy contents for backup: %w", err)
	}

	r.logger.Info("Successfully created a backup of the test set before modification.", zap.String("testSet", testSetID), zap.String("location", backupDestPath))
	return nil
}

// copyDirContents recursively copies contents from src to dst, excluding the .backup directory.
func (r *Replayer) copyDirContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		// CRITICAL: Exclude the backup directory itself to prevent recursion.
		if entry.Name() == ".backup" {
			continue
		}

		fileInfo, err := os.Stat(srcPath)
		if err != nil {
			return err
		}

		if fileInfo.IsDir() {
			if err := os.MkdirAll(dstPath, fileInfo.Mode()); err != nil {
				return err
			}
			if err := r.copyDirContents(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			// It's a file, copy it.
			srcFile, err := os.Open(srcPath)
			if err != nil {
				return err
			}
			defer srcFile.Close()

			dstFile, err := os.Create(dstPath)
			if err != nil {
				return err
			}
			defer dstFile.Close()

			if _, err := io.Copy(dstFile, srcFile); err != nil {
				return err
			}

			if err := os.Chmod(dstPath, fileInfo.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

// maxResetResends bounds how many times a transport-reset test request is
// re-sent. The reset is docker's userland-proxy resetting freshly-accepted
// host-port conns in bursts under load; under heavy CI contention that burst
// outlasts a couple of back-to-back attempts, so re-send work-slow — more
// attempts, spread by resetResendBackoff, to ride the burst out. Each attempt is
// gated on no-mock-consumed + port readiness, and a non-reset failure (genuinely
// broken app) still stops immediately, so the extra attempts only ever add
// latency on the reset path.
const maxResetResends = 6

// resetResendBackoff grows the pause between reset re-sends (attempt*backoff) so
// the bounded attempts spread across the docker-proxy reset window instead of
// hammering it back-to-back.
const resetResendBackoff = 300 * time.Millisecond

// resetResendReadyTimeout caps the per-attempt app-port readiness re-poll done
// before re-sending. It only ever adds latency on the (rare) reset path.
const resetResendReadyTimeout = 5 * time.Second

// retryResetOnce re-sends a test request that failed with a transport-level
// connection reset / unexpected EOF (origErr), but ONLY when doing so is
// provably safe. It returns (resp, true, nil) when a re-send produced a real
// response, and (nil, false, drained) when the request was not retried or every
// re-send also failed (the caller then treats origErr as the genuine failure).
//
// drained carries the mocks the safety gate observed-and-DRAINED for the
// just-failed request on the UNSAFE refusal path (see resetResendUnsafe and the
// drain-accounting note below); it is nil on every other path. The caller must
// fold it back into totalConsumedMocks so the agent's next-iteration filter
// (filterOutDeleted) still removes those exhausted single-use mocks.
//
// Safety invariant (this is the whole point): a reset is ambiguous about
// whether the app already consumed a single-use mock. MockManager.GetConsumed-
// Mocks is per-call and DRAINING — it returns only the mocks consumed SINCE the
// last drain, then clears its list. In the normal flow the per-test drain at
// RunTestSet (the `GetConsumedMocks` after a successful request) already emptied
// that list, so when we get here for a failed request the gate sees EXACTLY this
// request's consumption. We therefore re-send only while that per-call count is
// ZERO — i.e. THIS request consumed nothing irreversible. The moment it is >0 we
// stop and let the original error stand, so a mid-stream reset that already
// burned a mock is never re-run against exhausted mocks (which would fabricate a
// verdict). Under the dominant docker-proxy reset the app never saw the request,
// so the count stays at 0 and the re-send recovers the real response instead of
// a false status_code=0.
//
// Drain side-effect accounting: calling GetConsumedMocks in the gate DRAINS the
// agent's consumed-reporting list. This does NOT resurrect any exhausted mock —
// the agent serves from its mock TREES (DeleteFilteredMock removes a single-use
// mock from them on consume); the drained list is a separate keploy-side
// REPORTING channel. So a later test can never be re-served a drained-but-
// consumed mock by the agent's own state. The only risk is keploy-side: those
// drained mocks would be missing from totalConsumedMocks, so the next
// SendMockFilterParamsToAgent's filterOutDeleted would fail to filter them and
// SetMocksWithWindow could re-insert them into the serving pool. We close that
// by returning the drained mocks (UNSAFE path) for the caller to fold into
// totalConsumedMocks. On the SAFE path (count==0) the drain is a no-op, and the
// successful re-send's consumption is still accounted by the subsequent per-test
// GetConsumedMocks in RunTestSet (loopErr becomes nil, so that block runs).
func (r *Replayer) retryResetOnce(ctx context.Context, testCase *models.TestCase, testSetID string, origErr error) (interface{}, bool, []models.MockState) {
	// The gate below is the agent's word that the request consumed no mock,
	// which proves the app never processed it only while its dependencies are
	// mocks. At a base path they are real and no agent can give that word: a
	// request the app may have processed is never sent twice.
	if r.noAgent {
		return nil, false, nil
	}
	// A reset at an address the app can never be reached at (a published port
	// it listens behind only on 127.0.0.1 in its container) comes back on every
	// re-send, and waiting for it to serve first would wait out each gate.
	if pkg.IsUnreachableAppPort(origErr) {
		return nil, false, nil
	}
	for attempt := 1; attempt <= maxResetResends; attempt++ {
		if ctx.Err() != nil {
			return nil, false, nil
		}

		// Refuse to retry if THIS request consumed a new mock (per-call count
		// > 0 since the last drain) — that means it was (at least partially)
		// processed and a re-send would not be idempotent. Hand the drained
		// mocks back so the caller can keep totalConsumedMocks accurate.
		if unsafe, drained := r.resetResendUnsafe(ctx); unsafe {
			r.logger.Debug("transport reset is not safe to re-send (a mock was consumed); leaving the original error",
				zap.String("testSetID", testSetID),
				zap.String("testCaseID", testCase.Name))
			return nil, false, drained
		}

		// Re-gate on the app actually serving again (HTTP-level, on the same
		// endpoint the request dials) so we re-send only once the app/proxy is
		// ready, not blindly. Best effort: if we can't determine a probeable
		// target we proceed — the bounded attempts still protect us.
		r.waitForResetResendReady(ctx, testCase, testSetID)

		r.logger.Warn("test request reset by app/proxy before any mock was consumed; re-sending — the request was never processed, so this is not a retry of an assertion",
			zap.Int("attempt", attempt),
			zap.Int("maxRetries", maxResetResends),
			zap.String("testSetID", testSetID),
			zap.String("testCaseID", testCase.Name),
			zap.Error(origErr))

		// The test's capture window stays open across the re-send: what it
		// holds is this test's — the misses carried in from before it
		// (ContinueTestErrorCapture), those made as its mock window moved,
		// and any the failed try made (it consumed no mock) — and reopening
		// it would drop them.
		resp, err := r.hookImpl.SimulateRequest(ctx, testCase, testSetID)
		if err == nil {
			return resp, true, nil
		}
		if !pkg.IsTransportConnReset(err) {
			// A different failure on the re-send (e.g. the app really is down):
			// stop and let the caller report the original transport reset.
			return nil, false, nil
		}
		origErr = err
		// Back off (growing) before the next re-send so the bounded attempts
		// spread across the docker-proxy reset burst rather than hammering it.
		select {
		case <-ctx.Done():
			return nil, false, nil
		case <-time.After(time.Duration(attempt) * resetResendBackoff):
		}
	}
	return nil, false, nil
}

// resetResendUnsafe reports whether re-sending is unsafe because the failed
// request consumed at least one mock, and returns whatever it drained from the
// agent's consumed-reporting list so the caller can keep totalConsumedMocks
// accurate (see the drain side-effect note on retryResetOnce).
//
// GetConsumedMocks is per-call and DRAINING: it returns only the mocks consumed
// since the last drain (which, in the normal flow, is empty by the time we get
// here for a failed request). So the per-call result IS exactly this request's
// consumption, and the gate is unsafe iff that count is > 0. On a fetch error it
// returns unsafe=true with no drained mocks (fail safe): if we cannot prove
// nothing was consumed, we must not retry.
func (r *Replayer) resetResendUnsafe(ctx context.Context) (bool, []models.MockState) {
	consumed, err := r.hookImpl.GetConsumedMocks(ctx)
	if err != nil {
		r.logger.Debug("could not fetch consumed mocks to validate reset re-send; not retrying", zap.Error(err))
		return true, nil
	}
	if len(consumed) > 0 {
		return true, consumed
	}
	return false, nil
}

// waitForResetResendReady best-effort gates a reset re-send on the app actually
// serving again, bounded by resetResendReadyTimeout.
//
// It prefers an HTTP-level probe against the SAME endpoint the test request is
// dialed to — resolved via resolveProbeTarget, which mirrors the simulation's
// ResolveTestTarget (ConfigHost/--host, replaceWith, port precedence) rather than
// the raw recorded URL. This makes the gate address-family-correct (an IPv6
// "localhost" dial is gated on the IPv6 path, not a mismatched IPv4 127.0.0.1)
// and, unlike a bare TCP-accept gate, it does not declare readiness while
// docker's userland-proxy is still accepting-then-resetting connections
// mid-response. For a non-HTTP or unresolvable target it falls back to the docker
// published host-port TCP gate (no-op for native apps / unmapped publishes). On
// timeout it returns and lets the bounded re-send attempts run.
func (r *Replayer) waitForResetResendReady(ctx context.Context, testCase *models.TestCase, testSetID string) {
	// The same opt-out as the pre-test gate, and this path needs it more,
	// not less. Its trigger is a transport-level reset — which, behind a
	// kubectl port-forward, is exactly what a dying forward produces.
	// Re-opening a connect-then-close probe against the same address in
	// response is the one thing most likely to finish it off. The
	// bounded re-sends themselves are unaffected; only the readiness
	// re-gate is skipped. See config.Test.DisableAppReadyProbe.
	if r.config.Test.DisableAppReadyProbe {
		return
	}

	wctx, cancel := context.WithTimeout(ctx, resetResendReadyTimeout)
	defer cancel()

	if scheme, host, port, ok := resolveProbeTarget(r.config.Test, testCase, testSetID, r.logger); ok {
		if err := waitForHTTPServing(wctx, scheme, host, port); err != nil {
			r.logger.Debug("app not serving HTTP before reset re-send; re-sending anyway",
				zap.String("host", host), zap.String("port", port), zap.Error(err))
		}
		return
	}

	host, port, ok := dockerPublishedHostPort(r.config.Command)
	if !ok {
		return
	}
	if err := pkg.WaitForPort(wctx, host, port, resetResendReadyTimeout); err != nil {
		r.logger.Debug("app host port not ready before reset re-send; re-sending anyway",
			zap.String("host", host), zap.String("port", port), zap.Error(err))
	}
}

// openTestErrorCapture opens a test's mock-error capture window on the agent,
// before the agent's mock window moves to the test, on BOTH the normal and
// streaming paths; the matching attachMockErrors/GetMockErrors closes it (the
// streaming path only after the stream body is fully consumed). The first test
// of a set begins it (beginTestErrorCapture): what was missed before, the
// app's startup, is no test's. Every later test continues it: the misses made
// since the previous test's window closed, with no test running, are carried
// into this one, the test running when they can first be reported, rather
// than dropped. An agent without the continue capability begins instead.
// Best-effort: a failure only degrades to the old behaviour.
func (r *Replayer) openTestErrorCapture(ctx context.Context, opened *bool) {
	// The window is the agent's. A base path has none, and no mock to miss.
	if r.noAgent {
		return
	}
	first := !*opened
	*opened = true
	if !first {
		if c, ok := r.instrumentation.(interface {
			ContinueTestErrorCapture(context.Context) error
		}); ok {
			if err := c.ContinueTestErrorCapture(ctx); err != nil {
				r.logger.Debug("failed to continue test error capture", zap.Error(err))
			}
			return
		}
	}
	r.beginTestErrorCapture(ctx)
}

// beginTestErrorCapture opens a per-test mock-error capture window on the agent
// (via an optional capability — older agents / non-agent instrumentations skip
// it and fall back to the legacy global queue) so a mock miss during this test
// attributes to THIS test instead of whichever test drains GetMockErrors next,
// discarding what was missed with no window open.
// Best-effort: a failure only degrades to the old behaviour.
func (r *Replayer) beginTestErrorCapture(ctx context.Context) {
	if b, ok := r.instrumentation.(interface {
		BeginTestErrorCapture(context.Context) error
	}); ok {
		if err := b.BeginTestErrorCapture(ctx); err != nil {
			r.logger.Debug("failed to begin test error capture", zap.Error(err))
		}
	}
}

// attachMockErrors drains the per-test mock-error capture window (closing it)
// and records any unmatched outgoing calls onto the result and the end-of-run
// summary store. It must run on EVERY test-iteration exit — including the early
// simulation-error / invalid-response returns — so the window is finalized for
// THIS test and a miss is never carried forward to the next test or lost.
func (r *Replayer) attachMockErrors(ctx context.Context, testSetID, testCaseName string, result *models.TestResult) {
	r.attachReadMockErrors(r.readMockErrors(ctx), testSetID, testCaseName, result)
}

// testMisses is one read of a test's misses (readMockErrors): the calls no
// mock answered, and the read's error. asked is false when there was no agent
// to ask (a base path).
type testMisses struct {
	calls []models.UnmatchedCall
	err   error
	asked bool
	// lost and lostErr are, under the proven switch, what the agent could not
	// capture that the test is charged (testCaptureLosses: a taint since the
	// test set began, a failed TLS handshake since the previous test), and
	// why that could not be told.
	lost    models.CaptureLosses
	lostErr error
	// real are the test's REAL rows, under the proven switch with a
	// mock.real list (readRealTest); realLost the connections keploy could
	// neither capture nor relay as real while it ran, as a capture loss
	// (models.RealUncapturedLosses), and realErr why they could not be read.
	real     []models.RealUse
	realLost models.CaptureLosses
	realErr  error
}

// readMockErrors reads, once, the calls of the test just run that no mock
// answered. Under the proven switch the main loop reads them before its
// verdict, which they are part of, and attaches that read afterwards.
func (r *Replayer) readMockErrors(ctx context.Context) testMisses {
	// A miss is the agent's to report. A base path has none, and no mocks.
	if r.noAgent {
		return testMisses{}
	}
	calls, err := r.instrumentation.GetMockErrors(ctx)
	// What was missed before the set's first test is the set's, not this
	// test's (before_set_calls.go).
	calls = r.setCalls.take(calls)
	read := testMisses{calls: calls, err: err, asked: true}
	if provenswitch.On() {
		read.lost, read.lostErr = r.testCaptureLosses(ctx)
	}
	r.readRealTest(ctx, &read)
	return read
}

// attachReadMockErrors attaches a read of a test's misses to its result.
func (r *Replayer) attachReadMockErrors(read testMisses, testSetID, testCaseName string, result *models.TestResult) {
	if !read.asked {
		return
	}
	if len(read.real) > 0 && result != nil {
		result.Real = read.real
	}
	mockErrors, err := read.calls, read.err
	if err != nil {
		// A test whose misses could not all be read is marked, and whatever
		// was read is still attached. Under the proven switch a miss fails its
		// test, so such a test has no verdict and its test set is not judged
		// (UnmatchedCallsUnread; see testsWithUnreadMisses). With the switch
		// off the set is judged as before, but the test is marked
		// MissesUnattributed: a miss may be missing from it, or have been read
		// with another test's, so nothing may take a failed difference of its
		// set for noise (enterprise's auto-noise reads the mark). It was
		// dropped at Debug, and a miss that went unread was then masked.
		if provenswitch.On() {
			markUnmatchedCallsUnread(result)
		} else if result != nil {
			result.FailureInfo.MissesUnattributed = true
		}
		fields := []zap.Field{
			zap.String("testSetID", testSetID),
			zap.String("testCaseID", testCaseName),
			zap.Int("callsRead", len(mockErrors)),
			zap.Error(err),
		}
		if provenswitch.On() {
			r.logger.Error("could not read every call this test made that no mock answered; its test set is not judged", fields...)
		} else {
			r.logger.Warn("could not read every call this test made that no mock answered; a miss may be missing from this test, or have been read with another test's", fields...)
		}
	}
	for _, me := range mockErrors {
		if result != nil {
			result.FailureInfo.UnmatchedCalls = append(result.FailureInfo.UnmatchedCalls, me)
		}
		r.mockMismatchFailures.AddUnmatchedCallForTest(testSetID, testCaseName, me)
	}
	switch {
	case read.lostErr != nil:
		// The test's capture cannot be told: like a miss list not read
		// whole, it leaves the test without a verdict, and its set unjudged.
		if result != nil {
			result.FailureInfo.CaptureUnread = true
		}
		r.logger.Error("could not tell whether keploy's agent captured every call while this test ran; its test set is not judged",
			zap.String("testSetID", testSetID), zap.String("testCaseID", testCaseName), zap.Error(read.lostErr))
	case len(read.lost) > 0 && result != nil:
		// A call keploy cannot capture at all (decision 20) leaves the test
		// without a verdict; any other loss fails it.
		result.FailureInfo.CaptureIncomplete, result.FailureInfo.CannotCapture = read.lost.Split()
	}
	// The connections keploy could neither capture nor relay as real while
	// the test ran: capture incomplete, as a loss is, so the test fails with
	// CAPTURE_INCOMPLETE and what to change, never as a miss.
	switch {
	case read.realErr != nil:
		if result != nil {
			result.FailureInfo.CaptureUnread = true
		}
		r.logger.Error("could not read from keploy's agent which mock.real connections it could not capture while this test ran; its test set is not judged",
			zap.String("testSetID", testSetID), zap.String("testCaseID", testCaseName), zap.Error(read.realErr))
	case len(read.realLost) > 0 && result != nil:
		result.FailureInfo.CaptureIncomplete = append(append(models.CaptureLosses(nil), result.FailureInfo.CaptureIncomplete...), read.realLost...)
	}
}

// The third return is the test-set's startup mock names — boot traffic that
// belongs to no test case. On the mapping-based path the agent loads strictly
// by name (Agent.loadPerTestMocks -> disk.LoadByNames), so these must be merged
// into every per-test name list or the app's boot mocks are absent from the
// pool. The timestamp path does not need them: it reloads the same mocks via
// disk.LoadBefore(firstWindowStart).
//
// Returned even when the mapping is not "meaningful", so a caller that falls
// back to timestamp filtering still sees them; harmless there, since that path
// ignores names entirely.
func (r *Replayer) determineMockingStrategy(ctx context.Context, testSetID string, isMappingEnabled bool) (bool, map[string][]models.MockEntry, []string) {
	// Default to timestamp-based strategy with empty mappings.
	defaultMappings := make(map[string][]models.MockEntry)

	if r.mappingDB == nil {
		r.logger.Debug("No mapping database available, using timestamp-based mock filtering strategy")
		return false, defaultMappings, nil
	}

	if !isMappingEnabled {
		// The calling function already logs this, so we don't log it again here.
		return false, defaultMappings, nil
	}

	// Startup mocks are read independently of the per-test mappings: they are
	// absent from `tests:` by design, so Get() cannot surface them. A read
	// failure here is not fatal — it costs the startup names, not the run.
	var startupNames []string
	if startup, serr := r.mappingDB.GetStartup(ctx, testSetID); serr != nil {
		r.logger.Debug("Failed to read startup mocks from mappings; continuing without them",
			zap.String("testSetID", testSetID),
			zap.Error(serr))
	} else {
		startupNames = (&models.Mapping{Startup: startup}).StartupMockNames()
	}

	// Try to get mappings from the database.
	expectedTestMockMappings, hasMeaningfulMappings, err := r.mappingDB.Get(ctx, testSetID)
	if err != nil {
		r.logger.Debug("Failed to get mappings, falling back to timestamp-based filtering",
			zap.String("testSetID", testSetID),
			zap.Error(err))
		return false, defaultMappings, startupNames
	}

	if hasMeaningfulMappings {
		// Meaningful mappings were found, so use the mapping-based strategy.
		r.logger.Debug("Using mapping-based mock filtering strategy",
			zap.String("testSetID", testSetID),
			zap.Int("totalMappings", len(expectedTestMockMappings)),
			zap.Int("startupMocks", len(startupNames)))
		return true, expectedTestMockMappings, startupNames
	}

	// No meaningful mappings were found, so fall back to the timestamp-based strategy.
	//
	// NOTE: a test set whose ONLY outgoing traffic is startup traffic still
	// lands here — hasMeaningfulMappings counts per-test entries only. That is
	// deliberate: mapping-based selection exists to pick mocks PER TEST, and
	// with no per-test entry there is nothing to select. Such a set keeps the
	// timestamp path, which loads startup mocks via LoadBefore anyway.
	r.logger.Debug("No meaningful mappings found, using timestamp-based mock filtering strategy (legacy approach)",
		zap.String("testSetID", testSetID))
	return false, defaultMappings, startupNames
}

// isMockSubset checks if all expected mocks are present in the actual mocks list
// isReusableTierMock reports whether a loaded mock is a reusable, non-per-test
// tier — session / connection / config — recorded once at app startup and
// shared across every test rather than consumed by exactly one. Such mocks
// belong in the per-test mapping (so the replay mock pool is complete) but
// must be excluded from the per-test consumed-vs-expected assertion: they are
// not deterministically attributed to a single test's window, so comparing
// them would falsely demote tests to OBSOLETE. Only per-test mocks are
// compared. Tier is read from TestModeInfo.Lifetime (derived at ingest) with
// Spec.Metadata["type"] as a fallback for mocks whose lifetime wasn't derived.
func isReusableTierMock(m *models.Mock) bool {
	switch m.TestModeInfo.Lifetime {
	case models.LifetimeSession, models.LifetimeConnection:
		return true
	}
	if m.Spec.Metadata != nil {
		switch m.Spec.Metadata["type"] {
		case "config", "connection":
			return true
		}
	}
	return false
}

// rebalanceReusableMocks moves any mock that a MockMutator.AfterGetMocks
// re-tagged as reusable (isReusableTierMock — Lifetime Session/Connection or
// metadata type config/connection) out of the per-test `filtered` pool and into
// the reusable `unfiltered` pool, and returns the adjusted slices.
//
// It exists because AfterGetMocks receives the two pools BY VALUE: a mutator can
// retag a mock's Lifetime/metadata in place, but it cannot move a mock between
// the slices (a shrink of `filtered` or a grow of `unfiltered` does not
// propagate to the caller). Since the agent's SetMocksWithWindow window-filters
// every mock in `filtered` regardless of its per-mock tag, a mock retagged
// reusable but left in `filtered` would still be dropped when it fires outside
// its record-time window — defeating the retag. Running this immediately after
// AfterGetMocks honours the retag by physically re-partitioning.
//
// Memory: `filtered` is compacted in place (its backing array is reused via the
// [:0] read/write cursor); `unfiltered` grows only by the promoted count. No
// second copy of the corpus is allocated. Relative order within each pool is
// preserved. Safe on nil/empty inputs.
func rebalanceReusableMocks(filtered, unfiltered []*models.Mock) ([]*models.Mock, []*models.Mock) {
	if len(filtered) == 0 {
		return filtered, unfiltered
	}
	keep := filtered[:0]
	for _, m := range filtered {
		if m == nil {
			// A nil entry can never match; drop it from the per-test pool rather
			// than carrying it forward (SetMocksWithWindow skips nils anyway).
			continue
		}
		// A window-bound mock (a connection failure) stays per-test even when
		// a mutator re-tagged it reusable: moving it to the reusable pool
		// would serve a failure recorded in one test to every test. This
		// keploy never reaches it (its only caller has dropped the connection
		// failures, which it does not replay, by models.AgentBound); it is
		// here for the keploy that replays them, which keeps them in the pools.
		if isReusableTierMock(m) && !models.WindowBound(m.Kind) {
			unfiltered = append(unfiltered, m)
			continue
		}
		keep = append(keep, m)
	}
	return keep, unfiltered
}

// isReusableTierState is the MockState (consumed-mock) equivalent of
// isReusableTierMock. GetConsumedMocks carries the recorder-derived Lifetime
// and metadata type, so session/connection/config mocks are carved out of the
// consumed side of the assertion the same way they are on the expected side.
func isReusableTierState(s models.MockState) bool {
	switch s.Lifetime {
	case models.LifetimeSession, models.LifetimeConnection:
		return true
	}
	switch s.Type {
	case "config", "connection":
		return true
	}
	return false
}

// partitionInitialConsumed splits the mocks consumed BEFORE the first test of a
// test set fired into two tiers:
//
//   - startup: reusable/session traffic (driver handshakes, auth, connection-
//     pool warm-up) that belongs to no single test. It is folded into the
//     consumed accounting and written as the test set's STARTUP section.
//   - rearm: per-test single-use mocks that were consumed this early only
//     because the app-readiness gate (waitForAppReady) polled an endpoint that
//     calls a mocked dependency (e.g. a /health handler running SELECT 1 against
//     a mocked DB). These belong to a real recorded test case, so they must NOT
//     be marked consumed here; withholding them from totalConsumedMocks re-arms
//     them (the next SendMockFilterParamsToAgent re-inserts them into the
//     serving pool) so their owning test consumes them.
//
// The tier is decided by isReusableTierState on the recorder-derived
// Lifetime/type carried through GetConsumedMocks.
func partitionInitialConsumed(consumed []models.MockState) (startup, rearm []models.MockState) {
	// The carry-over mocks the agent flagged are split off before
	// (splitCarriedConsumed): consumed once, never re-armed.
	for _, m := range consumed {
		if isReusableTierState(m) {
			startup = append(startup, m)
		} else {
			rearm = append(rearm, m)
		}
	}
	return startup, rearm
}

// splitCarriedConsumed splits the mocks consumed before a set's first test into
// those the agent flagged CarryOver (a carry-over mock, consumed once) and the
// rest, which partitionInitialConsumed sorts.
func splitCarriedConsumed(consumed []models.MockState) (carried, rest []models.MockState) {
	for _, m := range consumed {
		if m.CarryOver {
			carried = append(carried, m)
			continue
		}
		rest = append(rest, m)
	}
	return carried, rest
}

func isMockSubset(actual []string, expected []string) bool {
	actualMap := make(map[string]bool)
	for _, mock := range actual {
		actualMap[mock] = true
	}

	for _, mock := range expected {
		if !actualMap[mock] {
			return false
		}
	}
	return true
}

// buildExpectedMockInfos converts the per-test expected mock list (from the
// recorded mappings) into the MockMismatchInfo.ExpectedMocks shape. Entries of
// a kind the per-test assertion leaves out (isUnassertedMockEntry: DNS, whose
// resolution order is non-deterministic, and connection failures, which
// nothing consumes at replay yet) are filtered out both at the entry level and
// via the mockKindByName lookup; listed here they would only ever show as
// spurious "expected, not consumed" entries. Resolves an empty Kind by looking
// up mockKindByName so the consumer always gets a kind label when one is
// available.
//
// Extracted from RunTestSet for unit-testability.
func buildExpectedMockInfos(expectedMocks []models.MockEntry, mockKindByName map[string]models.Kind) []models.MockMismatchMock {
	out := make([]models.MockMismatchMock, 0, len(expectedMocks))
	for _, m := range expectedMocks {
		if isUnassertedMockEntry(m, mockKindByName) {
			continue
		}
		resolvedKind := m.Kind
		if resolvedKind == "" {
			if kind, ok := mockKindByName[m.Name]; ok {
				resolvedKind = string(kind)
			}
		}
		out = append(out, models.MockMismatchMock{Name: m.Name, Kind: resolvedKind})
	}
	return out
}

// buildActualMockInfos converts the per-test consumed mock list (from the
// instrumentation/agent) into the MockMismatchInfo.ActualMocks shape.
// `known` gates the build: when false (e.g. GetConsumedMocks failed for THIS
// test in non-instrument mode) we return an empty slice rather than walking
// stale data attributed to the wrong test case. Kinds the per-test assertion
// leaves out (models.ExcludedFromDependencyAssertion) are filtered, as they are
// from the expected list.
//
// Extracted from RunTestSet for unit-testability.
func buildActualMockInfos(consumed []models.MockState, known bool) []models.MockMismatchMock {
	out := make([]models.MockMismatchMock, 0, len(consumed))
	if !known {
		return out
	}
	for _, m := range consumed {
		if models.ExcludedFromDependencyAssertion(m.Kind) {
			continue
		}
		out = append(out, models.MockMismatchMock{Name: m.Name, Kind: string(m.Kind)})
	}
	return out
}

// backfillStartupSection adds the startup section to mappings.yaml when the
// replay captured one and the file on disk has none.
//
// Needed because the section is otherwise unreachable in the default flow:
// `keploy record` writes mappings.yaml through UpsertBatch (no startup
// capture), so by the time `keploy test` runs the file EXISTS, the
// create-if-absent branch above is skipped, UpdateTestMapping defaults to
// false, and the Startup slice built earlier is computed and discarded.
//
// ⚠ Written as a STARTUP-ONLY document, deliberately NOT by flipping
// shouldWriteMappings. That flag routes through StoreMappings ->
// mapdb.Insert, and Insert REPLACES per-test entries
// (`finalMappings[t.ID] = t.Mocks`) for every test in the document. On a
// subset run — `keploy test --tests test-A` — actualTestMockMappings holds
// only test-A, populated from THIS run's consumption, so the operator's
// curated list for test-A would be overwritten with whatever this run
// happened to consume. A short-circuited or partly-failed run writes a
// strict subset as authoritative, and the next mapping-based run fails that
// test with no_mocks. That is exactly the contract the create-if-not-present
// gate exists to protect ("once a file exists, leave it alone; to refresh the
// passing tests' entries, pass --update-test-mapping").
//
// With TestCases empty, Insert's seed-from-existing loop carries every
// on-disk entry through untouched and only the section is added.
//
// Fires at most once per test set (gated on the section being genuinely
// absent), so it cannot churn the file. A read failure costs the section,
// not the run. RunTestSet calls it only for a file that exists: a file it
// created would map no test and block the create that does.
//
// Extracted from RunTestSet so the gate is unit-testable: it is the exact
// decision that made the whole feature unreachable in the default flow, and the
// first fix for it clobbered per-test mappings.
func (r *Replayer) backfillStartupSection(ctx context.Context, testSetID string, actual *models.Mapping) {
	if r.mappingDB == nil || actual == nil || len(actual.Startup) == 0 {
		return
	}

	onDisk, err := r.mappingDB.GetStartup(ctx, testSetID)
	if err != nil {
		r.logger.Debug("Skipping startup-section backfill — could not read the existing section",
			zap.String("testSetID", testSetID),
			zap.Error(err))
		return
	}
	if len(onDisk) > 0 {
		return
	}

	startupOnly := &models.Mapping{
		Version:   actual.Version,
		Kind:      actual.Kind,
		TestSetID: testSetID,
		Startup:   actual.Startup,
	}
	if err := r.mappingDB.Insert(ctx, startupOnly); err != nil {
		r.logger.Error("Error adding the startup section to mappings.yaml", zap.Error(err))
		return
	}
	r.logger.Info("Added the startup section to mappings.yaml",
		zap.String("testSetID", testSetID),
		zap.Int("startupMocks", len(actual.Startup)))
}

// streamingTest is a test case deferred to the sequential streaming phase.
//
// Package-scoped rather than local to RunTestSet so newStreamingTest is
// testable: the field below carries an invariant that is easy to break by
// "simplifying".
type streamingTest struct {
	testCase *models.TestCase
	// expectedMocks is PER-TEST ONLY, deliberately.
	//
	// It feeds isMockSubsetWithConfig, which compares this against what the test
	// consumed. The agent's list is built separately at the send site and DOES
	// include the test-set's startup names — merging them here instead would be a
	// false pass: a per-test-lifetime mock consumed at boot lands in `startup:`,
	// and having it in the expected set would tolerate that unexpected
	// consumption, letting a test that should go OBSOLETE pass.
	expectedMocks []string
}

// newStreamingTest builds the deferred entry for a streaming test case. It
// copies the test case because the caller's loop variable is reused.
func newStreamingTest(testCase *models.TestCase, mappings map[string][]models.MockEntry) streamingTest {
	tcCopy := *testCase
	entries := mappings[testCase.Name]
	names := make([]string, len(entries))
	for i, m := range entries {
		names[i] = m.Name
	}
	return streamingTest{testCase: &tcCopy, expectedMocks: names}
}
