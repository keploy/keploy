package replay

import (
	"context"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/utils"
)

// setEndDrainBudget bounds the read of what the app consumed as the set's
// end stopped it (P101-b): the agent's client has no deadline of its own, so
// a read that never ends would hold the run for good. A read that runs out
// leaves the set not judged.
var setEndDrainBudget = 30 * time.Second

// whyAppNotStopped says why keploy read a set's end with its application
// still running, under the proven switch: the set's report carries it
// (models.TestReport.AppNotStopped), as a call the app makes after that read
// is not judged (P101-c).
func whyAppNotStopped(cmdType utils.CmdType, serveTest bool) string {
	switch {
	case cmdType == utils.DockerCompose && serveTest:
		return "keploy keeps the application's docker compose stack running across test-sets and did not stop the application before reading the set's end, so a call it makes after that read is not judged"
	case cmdType == utils.DockerCompose:
		return "the application runs in a docker compose stack whose application's container keploy cannot stop apart from the stack, keploy's agent among it, so keploy did not stop the application before reading the set's end, and a call it makes after that read is not judged"
	case serveTest:
		return "--keep-app-alive keeps the application running across test-sets, so keploy did not stop it before reading the set's end, and a call it makes after that read is not judged; it is stopped before a read only at the last test-set of the run that runs (a later one wholly ignored, or with no test case, runs none), on an attempt no other follows"
	default:
		return "keploy did not start the application (no -c), so it could not stop it before reading the set's end, and a call it makes after that read is not judged"
	}
}

// whyStopNotRead says why the set's end read nothing of what its application
// consumed as it stopped: its docker compose app exited on its own before
// the set's end (halted: the set is APP_HALTED, and the exit, which its
// watch ends the run on, took keploy's agent down with the stack), or keploy
// did not stop it (whyAppNotStopped).
func whyStopNotRead(cmdType utils.CmdType, serveTest, halted bool) string {
	if halted {
		return "the application exited on its own before the set's end: the set is APP_HALTED, and the exit took keploy's agent down with the application's docker compose stack, so nothing the application consumed as it stopped could be read"
	}
	return whyAppNotStopped(cmdType, serveTest)
}

// awaitAppExit waits for exited, closed once the application keploy stopped
// has exited, as the drain named group waits for its goroutines
// (teardownDrain): for as long as scope's teardown moves, and, said as that
// drain's give-up, gives the application up once it has made no progress
// for bound (that drain's) of keploy's runnable time, or once ctx ends. It
// reports whether the application exited. A set's end that stops the
// application before it reads its last calls thus waits for a stop that
// moves, as long as it takes, but never for one wedged past what the set's
// drain would give it: an application whose stop ignores its cancel, waited
// for without a bound, held keploy test before the set's report for good.
func (r *Replayer) awaitAppExit(ctx context.Context, exited <-chan struct{}, scope *utils.TeardownScope, group string, bound time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-exited:
		case <-ctx.Done():
		}
	}()
	utils.AwaitProgressOn(r.drainClock, r.logger, group, done, bound, scope.Moves)
	select {
	case <-exited:
		return true
	default:
		return false
	}
}

// wholeSetIgnored reports whether test.ignoredTests ignores all of the set
// id: it names the set with no test case. RunTestSet returns for such a set
// before it begins the set's session on the agent.
func wholeSetIgnored(ignored map[string][]string, id string) bool {
	tests, ok := ignored[id]
	return ok && len(tests) == 0
}

// prescannedSet holds the single GetTestCases result that lastSetThatRuns
// already loaded on the fallback path (stores that do not opt into
// TestCasePresenceReader). Bounded to one test set and consumed once on that
// set's first RunTestSet attempt so neither cases nor read errors are fetched
// or logged twice.
type prescannedSet struct {
	id    string
	cases []*models.TestCase
	err   error
}

// presenceReaderOf returns db as a TestCasePresenceReader only when db
// explicitly opts in for its own concrete value (PresenceAppliesTo). A
// wrapper that embeds *testdb.TestYaml and overrides GetTestCases inherits
// both methods by promotion, fails PresenceAppliesTo(db) because the promoted
// receiver is the inner *TestYaml rather than db, and safely falls back to
// GetTestCases.
func presenceReaderOf(db TestDB) TestCasePresenceReader {
	reader, ok := db.(TestCasePresenceReader)
	if !ok || !reader.PresenceAppliesTo(db) {
		return nil
	}
	return reader
}

func (r *Replayer) clearPrescanned() {
	r.prescanned = nil
	r.prescannedEmpty = nil
}

// loadTestCases returns testSetID's test cases for RunTestSet, consuming a
// pre-scanned fallback result from lastSetThatRuns once (on the set's first
// attempt) so fallback stores neither decode/fetch nor log a read error twice.
// Subsequent attempts (flaky retries, --must-pass after deleting failed cases)
// always read fresh from r.testDB.
func (r *Replayer) loadTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error) {
	if r.prescanned != nil && r.prescanned.id == testSetID {
		cached := r.prescanned
		r.prescanned = nil
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return cached.cases, cached.err
	}
	if _, ok := r.prescannedEmpty[testSetID]; ok {
		delete(r.prescannedEmpty, testSetID)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return r.testDB.GetTestCases(ctx, testSetID)
}

// lastSetThatRuns is the index in testSets of the last set that begins its
// session on the agent (noLaterSetRuns), -1 for none: the last test case it
// sends (lastSentOf) carries KEPLOY-LAST-TESTCASE, and its final attempt
// stops the application --keep-app-alive keeps across sets (P101-c), or,
// with the proven switch off, a docker compose stack reused across them
// (stopForSetEndRead). A set
// wholly ignored (wholeSetIgnored), or with no test case, returns before it
// begins one (RunTestSet) and runs nothing of the app's: a stop at the run's
// teardown, after every read, would leave its closes in no verdict, and no
// test case would carry KEPLOY-LAST-TESTCASE.
// A set whose test cases cannot be read here counts as one that runs:
// stopping the app before a set that runs would fail its tests, and
// RunTestSet fails one that cannot be read. So do the two other sets that
// return before their session, each FAILED, which fails the run: one refused
// as half-written (refuseInterruptedSet) and one none of whose -t-named test
// cases is recorded (failSetUnrun). After a trailing one of those, no
// request carries the header and the kept application stops only in the
// run's teardown, on a run that has already failed.
func (r *Replayer) lastSetThatRuns(ctx context.Context, testSets []string) int {
	r.clearPrescanned()
	reader := presenceReaderOf(r.testDB)
	for i := len(testSets) - 1; i >= 0; i-- {
		setID := testSets[i]
		if wholeSetIgnored(r.config.Test.IgnoredTests, setID) {
			continue
		}
		if reader != nil {
			has, err := reader.HasTestCases(ctx, setID)
			if err == nil && !has {
				continue
			}
			return i
		}
		cases, err := r.testDB.GetTestCases(ctx, setID)
		if err == nil && len(cases) == 0 {
			if r.prescannedEmpty == nil {
				r.prescannedEmpty = make(map[string]struct{})
			}
			r.prescannedEmpty[setID] = struct{}{}
			continue
		}
		// Keep at most the one settled set so RunTestSet reuses it once
		// without fetching/decoding or logging an error a second time.
		r.prescanned = &prescannedSet{id: setID, cases: cases, err: err}
		return i
	}
	return -1
}

// lastSentOf is the test case of tests, in the order a pass of the set runs
// them, whose request the pass sends last, nil when it sends none. In the last
// set that runs (noLaterSetRuns) it carries KEPLOY-LAST-TESTCASE: never a test
// case the pass fails unsent, which sends no request. The pass fails unsent
// one refused when the set loaded (refused), and one whose URL the base path
// cannot replace (requestAtBasePath, as applyBasePath reads it). That URL is
// read as the set loaded it, before BeforeTestCaseRun, which only decrypts
// the values in it, never its scheme and host; a retry cycle's pass runs only
// test cases an earlier pass sent, the base path in their URLs already. A
// pass's tests hold no test case outside -t's named ones, nor an ignored one:
// RunTestSet leaves those out before its first pass. A set that sends none of
// its test cases has failed each one, so a run whose last set that runs is
// such a set sends no header, on a run that has already failed.
func (r *Replayer) lastSentOf(tests []*models.TestCase, refused map[string][]templates.Refusal) *models.TestCase {
	for i := len(tests) - 1; i >= 0; i-- {
		tc := tests[i]
		if len(refused[tc.Name]) > 0 {
			continue
		}
		if r.config.Test.BasePath != "" && tc.Kind == models.HTTP {
			if _, err := requestAtBasePath(r.config.Test.BasePath, tc.HTTPReq); err != nil {
				continue
			}
		}
		return tc
	}
	return nil
}

// keepAliveStopper gives the app --keep-app-alive keeps across a run's sets a
// context of its own, and the stop of it the last set's final attempt calls
// before its end's read (P101-c): stop cancels the app's context and waits
// with await (awaitAppExit) until ran has been told how its run ended, then
// says why its stop had to kill it ("" when it did not, or when await gave
// the app up: the run's ctx ended first, or its stop made no progress for
// its bound). Under the
// proven switch the cancel's cause is models.ErrAppStoppedForSetEnd; with it
// off it is a plain cancel, the stop the run's end gives the app, and the
// read it comes before is of the calls the app made after the run's last
// test, for the set's report alone (stopForSetEndRead). Not under docker
// compose, where the end of the app's run takes its stack down, keploy's
// agent with it: stop is nil there, and appCtx is ctx; the set's end stops
// the app's container itself, the stack running on (composeAppStopper).
func keepAliveStopper(ctx context.Context, cmdType utils.CmdType, await func(ctx context.Context, exited <-chan struct{}) bool) (appCtx context.Context, stop func() string, ran func(models.AppError)) {
	if cmdType == utils.DockerCompose {
		return ctx, nil, func(models.AppError) {}
	}
	var cause error
	if provenswitch.On() {
		cause = models.ErrAppStoppedForSetEnd
	}
	appCtx, cancel := context.WithCancelCause(ctx)
	exited := make(chan struct{})
	var killed string
	stop = func() string {
		cancel(cause)
		if await(ctx, exited) {
			return killed
		}
		return ""
	}
	ran = func(appErr models.AppError) {
		killed = appErr.StopKilled
		close(exited)
	}
	return appCtx, stop, ran
}

// noAttemptFollows reports whether attempt is the last a set can be given:
// the last flaky check, and no --must-pass, which runs a set again after its
// failing tests are taken out, however many attempts it has had.
func noAttemptFollows(attempt int, test config.Test) bool {
	return attempt == int(test.MaxFlakyChecks) && !test.MustPass
}

// composeAppStopper is an instrumentation that stops and starts the app's
// container of a docker compose stack, its stack running on (AgentClient;
// P101-c): at the set's end under the proven switch, and before the set's
// report with it off (stopAppBeforeReport).
type composeAppStopper interface {
	StopComposeApp(ctx context.Context) (string, error)
	// StartComposeApp says what keploy read of the container as it started
	// it: one that was not running (a new app), one found running, or one
	// the docker daemon gave no answer of. anew is called before the start
	// where the container was read not running.
	StartComposeApp(ctx context.Context, anew func()) (models.ComposeAppStart, error)
}
