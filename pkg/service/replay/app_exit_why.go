package replay

import (
	"context"
	"fmt"
	"sync"
	"time"

	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// unansweredTest is the test a set sent its application last that the
// application has not answered yet: the test an exit of the application held,
// which the set's report names (appExitWhy). A set sends its tests one at a
// time, so a test sent replaces the one before it, answered or not.
type unansweredTest struct {
	mu   sync.Mutex
	name string
}

// sent marks the test named sent to the application.
func (u *unansweredTest) sent(name string) {
	u.mu.Lock()
	u.name = name
	u.mu.Unlock()
}

// answered marks the test named answered: an exit of the application from
// now on did not hold it.
func (u *unansweredTest) answered(name string) {
	u.mu.Lock()
	if u.name == name {
		u.name = ""
	}
	u.mu.Unlock()
}

// held is the test sent and not answered, "" when there is none.
func (u *unansweredTest) held() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.name
}

// appExitWhy says how the application's own exit stopped its set, for the
// set's report: the exit code, where the run knows it, and the test the
// application had not answered when keploy learned of the exit (held). It is
// "" for an end of the run that is no exit of the application's own: keploy's
// stop of it is a cancel (models.ErrCtxCanceled).
func appExitWhy(e models.AppError, held string) string {
	if e == (models.AppError{}) || e.AppErrorType == models.ErrCtxCanceled {
		return ""
	}
	why := "it exited"
	switch {
	case e.ExitCode > 0:
		why += fmt.Sprintf(" with code %d", e.ExitCode)
	case e.AppErrorType == models.ErrAppStopped:
		// The runners say a clean exit so: its code is 0. Another end
		// with no code above 0 is one whose code the run does not know.
		why += " with code 0"
	}
	if held != "" {
		why += " before it answered test " + held
	}
	return why
}

// keptAppExit is an exit of its own of the application keploy runs once for
// the whole run (a compose stack kept across sets, --keep-app-alive), as its
// runner tells it at the exit (models.WithAppExit): the set running then
// halts on it at once, and every later set finds it told. Under docker
// compose the runner tells it at the app's container's die, while compose's
// up still waits out the rest of the stack: until then keploy took the app
// for running.
type keptAppExit struct {
	once sync.Once
	// told is closed once the exit is told; exit is then set.
	told chan struct{}
	exit models.AppError

	mu sync.Mutex
	// running is the set running now ("" between sets) and last the set
	// that ran before it, as begin and its end mark them.
	running, last string
	// isTold is whether the exit is told, and toldIn and toldAfter the
	// set running and the one that had ended last when it was.
	isTold            bool
	toldIn, toldAfter string
}

func newKeptAppExit() *keptAppExit {
	return &keptAppExit{told: make(chan struct{})}
}

// tell keeps the exit e, once: the first word is the exit's.
func (k *keptAppExit) tell(e models.AppError) {
	k.once.Do(func() {
		k.mu.Lock()
		k.exit = e
		k.isTold, k.toldIn, k.toldAfter = true, k.running, k.last
		k.mu.Unlock()
		close(k.told)
	})
}

// begin marks the set named running until end, and says whether the app's
// exit was told before the set began (gone) and, if so, when (earlier,
// earlierExitWhy): a set that begins after the app has gone is not the app's
// startup, unless no set ran before the exit. begin and tell take the same
// lock, so an exit is told either before the set began, and said here, or
// while it runs.
func (k *keptAppExit) begin(set string) (gone bool, earlier string, end func()) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.running = set
	if k.isTold {
		gone, earlier = true, earlierExitWhy(set, k.toldIn, k.toldAfter)
	}
	return gone, earlier, func() {
		k.mu.Lock()
		k.running, k.last = "", set
		k.mu.Unlock()
	}
}

// toldExit is the exit, once told (told false until then), and whether a
// set had ended before it was told (afterASet): the app had served a set, so
// an exit before a set's first test is not its startup failing.
func (k *keptAppExit) toldExit() (e models.AppError, told, afterASet bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.exit, k.isTold, k.toldAfter != ""
}

// earlierExitWhy says when an exit told before set began was told: while the
// set in ran, or after the set after had ended. A run that tries a set again
// runs it twice, and the earlier try is said so. "" for an exit told before
// any set began: that is the app's startup failing.
func earlierExitWhy(set, in, after string) string {
	switch {
	case in == set:
		return "while an earlier attempt of this test set ran"
	case in != "":
		return "while test set " + in + " ran"
	case after == set:
		return "after an earlier attempt of this test set ended"
	case after != "":
		return "after test set " + after + " ended"
	}
	return ""
}

// listenedTo is ctx carrying k's tell (models.WithAppExit), with k: the
// context the run's application runs under.
func listenedTo(ctx context.Context) (context.Context, *keptAppExit) {
	k := newKeptAppExit()
	return models.WithAppExit(ctx, k.tell), k
}

// reportHaltedBeforeTests ends a set the application's exit halted before
// its first test: it ran none of its tests, and its report says the app's
// exit and status (mapAppErrorToTestSetStatus), as shape words it (why, the
// log's message, says it in keploy's log): a set that began after the exit
// of the app the run keeps for every set was told (keptAppExit.begin,
// shape.exitedEarlier), or one whose setup the exit ended (setupEnd in
// RunTestSet, shape.exitedInSetup), or the app's startup failing. The set is
// counted in the run's totals as one whose app halted before its first test
// is.
func (r *Replayer) reportHaltedBeforeTests(ctx context.Context, testRunID, testSetID string, testCases []*models.TestCase, startTime time.Time, status models.TestSetStatus, exit models.AppError, shape runShape, why string) (models.TestSetStatus, error) {
	total := len(testCases)
	if selected := matcherUtils.ArrayToMap(r.config.Test.SelectedTests[testSetID]); len(selected) != 0 {
		total = len(selected)
	}
	shape.appExit = appExitWhy(exit, "")
	reason := describeTestSetFailure(status, nil, shape)
	r.logger.Error(why, zap.String("test-set", testSetID), zap.String("reason", reason))
	appLogs := exit.AppLogs
	if appLogs == "" && shouldIncludeAppLogs(status) {
		logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		appLogs = r.instrumentation.GetRecentAppLogs(logCtx)
		cancel()
	}
	timeTaken := time.Since(startTime)
	testReport := &models.TestReport{
		Version:       models.GetVersion(),
		TestSet:       testSetID,
		Status:        string(status),
		Total:         total,
		TimeTaken:     timeTaken.String(),
		FailureReason: reason,
		AppLogs:       appLogs,
		CmdUsed:       r.config.Test.CmdUsed,
		Proven:        provenswitch.On(),
	}
	r.noteSetEnd(testReport)
	if err := r.reportDB.InsertReport(context.WithoutCancel(ctx), testRunID, testSetID, testReport); err != nil {
		utils.LogError(r.logger, err, "failed to insert report")
		return models.TestSetStatusInternalErr, fmt.Errorf("failed to insert report")
	}
	// It said why it ran none, above: that it has no results to read is not
	// another error (saysResultsUnread).
	r.markNeverReady(testSetID)

	r.completeTestReportMu.Lock()
	r.completeTestReport[testSetID] = TestReportVerdict{
		total:     total,
		duration:  timeTaken,
		timeTaken: timeTaken.String(),
	}
	r.totalTests += total
	r.totalTestTimeTaken += timeTaken
	r.completeTestReportMu.Unlock()
	return status, nil
}

// statusOnExit is the status of a set whose watchers set theirs (set), once
// keploy knows (exited) of an exit of the app's own whose status is exit: the
// exit's. The app's listener keeps the exit as it is told, and the reader
// that ends the set on it sets the set's status only after, so a set read
// between the two reads as it does after.
func statusOnExit(set, exit models.TestSetStatus, exited bool) models.TestSetStatus {
	if exited {
		return exit
	}
	return set
}
