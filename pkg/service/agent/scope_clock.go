package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// TestClock is an optional extension of AgentHooks, for a build that freezes
// the clock of the run's processes. Under the proven switch, a replay moves
// that clock to each test's recorded begin while the test is open, and back
// to the run's start time when it ends (design v14 §3.4): a test sees the
// instant its recording saw, whatever ran before it. The start time is the
// anchor SetFreezeAnchor gave the build.
//
// The agent calls both under its clock lock, one move at a time; an
// implementation must not call back into the agent.
type TestClock interface {
	// BeginTestClock moves the run's clock to at, when test began in its
	// recording. A zero at (a recording made before keploy kept it) keeps the
	// run's start time.
	BeginTestClock(ctx context.Context, test string, at time.Time) error
	// EndTestClock moves the run's clock back to its start time.
	EndTestClock(ctx context.Context, test string) error
}

// RealTestClock is an optional extension of TestClock, for a build that can
// put the run's clock on real time while a test is open: a test whose
// recording lists a mock.real destination runs on the real clock, in every
// replay (design v14 §3.4, F3), since the real dependency answers at real
// time. EndTestClock moves the clock back to the run's start time after it,
// as after any test. The agent calls it under its clock lock, as it calls
// BeginTestClock.
type RealTestClock interface {
	BeginRealTestClock(ctx context.Context, test string) error
}

// TimeFiler is an optional extension of AgentHooks: the clock record the
// run's processes read through KEPLOY_TIME_FILE.
type TimeFiler interface {
	TimeFile() string
}

// TimeFile is the clock record the run's processes read, which a test
// process's keploy integration exports as KEPLOY_TIME_FILE; "" when this
// agent does not freeze the run. The single-worker wire's claim answers it
// (the timeFile of POST /agent/instance, release1-cut §2.1); the enterprise
// build implements TimeFiler.
func TimeFile() string {
	if f, ok := ActiveHooks.(TimeFiler); ok {
		return f.TimeFile()
	}
	return ""
}

// ErrNoTestClock is SetTestBegins' refusal under the proven switch when the
// build freezes the run's clock (TimeFiler) but cannot move it per test
// (TestClock): its tests would all run at the start time, not at their
// recorded begins, with no word.
var ErrNoTestClock = errors.New("this keploy agent freezes the run's clock but cannot move it to each test's recorded begin")

// ErrNoRealTestClock is SetTestBegins' refusal under the proven switch when
// the build freezes the run's clock but cannot put it on real time for a test
// whose recording lists a mock.real destination (RealTestClock): the test
// would run at its recorded begin against a dependency that answers at real
// time.
var ErrNoRealTestClock = errors.New("this keploy agent freezes the run's clock but cannot put it on real time for a test that uses mock.real")

// SetTestBegins installs when each test of the replayed recording began, by
// test name, and the mock.real destinations each test's recording lists
// (real): those tests run on the real clock. The CLI installs them for a
// replay session, like the scope table; a new session clears them
// (resetScopeState).
func (a *Agent) SetTestBegins(_ context.Context, begins map[string]time.Time, real map[string][]string) error {
	if provenswitch.On() && TimeFile() != "" {
		if _, ok := ActiveHooks.(TestClock); !ok {
			return ErrNoTestClock
		}
		if _, ok := ActiveHooks.(RealTestClock); !ok && anyReal(real) {
			return ErrNoRealTestClock
		}
	}
	a.scopeMu.Lock()
	a.testBegins = maps.Clone(begins)
	a.testReal = nil
	for test, dests := range real {
		if len(dests) == 0 {
			continue
		}
		if a.testReal == nil {
			a.testReal = map[string][]string{}
		}
		a.testReal[test] = slices.Clone(dests)
	}
	a.scopeMu.Unlock()
	return nil
}

// anyReal reports whether any test lists a mock.real destination.
func anyReal(real map[string][]string) bool {
	for _, dests := range real {
		if len(dests) > 0 {
			return true
		}
	}
	return false
}

// testClock is the build's TestClock when the proven switch is on.
func testClock() (TestClock, bool) {
	if !provenswitch.On() {
		return nil, false
	}
	c, ok := ActiveHooks.(TestClock)
	return c, ok
}

// ErrOneTestAtATime is beginTestClock's refusal of a test that begins while
// another holds the run's clock (release1-cut §2.1: 422 one_test_at_a_time).
var ErrOneTestAtATime = errors.New("the run has one clock, and keploy runs one test at a time on it")

// beginTestClock moves the run's clock to test's recorded begin, and reports
// whether test took the clock. The clock is the run's, so one test holds it
// at a time: a test that begins while another holds it fails
// (ErrOneTestAtATime), as the single-worker wire refuses a second open test
// (design v14 §5.1). A subtest of the holder (its name under the holder's,
// "TestX/case", from the same worker) runs on the holder's clock, and a suite
// scope does not move it. A clock that cannot be moved fails the begin: the
// test would run on another clock than its recording's, and pass or fail for
// that.
//
// A holder whose worker has exited (a test that called os.Exit, a test
// binary killed by its -timeout) never sends its end. When a test begins and
// the holder's worker is gone (procStartTime no longer gives the start time
// it had at the holder's begin: exited, a zombie, or its pid is another
// process's now, the new test's own included), the holder is lost: begin
// returns it, to be recorded failed, and the new test takes the clock. A
// begin from the holder's own living worker is refused: keploy cannot tell a
// holder whose end was lost from a test the worker runs beside it (Go's
// t.Parallel), and taking the clock would run the holder on another test's
// clock to a verdict that does not say so. A holder keploy cannot see (a
// harness that sends no pid, or one in another pid namespace than the
// agent's) is never judged lost. The next session (releaseTestClock) frees
// the clock either way. The single-worker wire's claim (release1-cut §2.1)
// is where a pid-less harness's process will be known.
func (a *Agent) beginTestClock(ctx context.Context, test string, pid int) (took bool, lost lostScope, err error) {
	c, ok := testClock()
	if !ok {
		return false, lostScope{}, nil
	}
	k := scopeKey{pid: uint32(pid), name: test}
	a.scopeMu.Lock()
	suite := a.scopeMeta[k].suite
	at := a.testBegins[test]
	real := len(a.testReal[test]) > 0
	a.scopeMu.Unlock()
	if suite {
		return false, lostScope{}, nil
	}
	start := procStartTime(pid)
	a.clockMu.Lock()
	defer a.clockMu.Unlock()
	if held := a.clockHeld; held.name != "" {
		// The holder's worker is gone when its start time was known at its
		// begin and is not now; a new test from the holder's pid with
		// another start time is a new process on a reused pid.
		gone := a.clockHeldStart != "" && workerGone(int(held.pid), a.clockHeldStart)
		if !gone && held.pid == k.pid && (held.name == test || strings.HasPrefix(test, held.name+"/")) {
			return false, lostScope{}, nil
		}
		if !gone {
			err := fmt.Errorf("%s began while %s was open: %w", test, held.name, ErrOneTestAtATime)
			a.logger.Error("keploy refused a test: another test holds the run's clock", zap.String("test", test), zap.Int("worker", pid),
				zap.String("open", held.name), zap.Uint32("openWorker", held.pid), zap.Error(err),
				zap.String("next_step", "run the tests one at a time (no parallel tests in one worker), and make the harness end each test it begins"))
			return false, lostScope{}, err
		}
		lost = lostScope{key: held, start: a.clockHeldStart}
		a.logger.Warn("the test that held the run's clock ended without its end: its worker exited; it is recorded failed",
			zap.String("test", held.name), zap.Uint32("worker", held.pid), zap.String("next", test))
	}
	// A move is absolute: it also ends a move an earlier end could not undo.
	// One that fails leaves the clock as it was, and what is in force stays
	// marked. A test that uses mock.real runs on the real clock (F3).
	if err := beginClock(ctx, c, test, at, real); err != nil {
		a.logger.Error("keploy could not move the run's clock to when the test began in its recording; the test fails", zap.String("test", test), zap.Error(err))
		if lost.key.name != "" {
			// The lost holder is still lost: free the clock of it.
			a.clockHeld, a.clockHeldStart = scopeKey{}, ""
		}
		return false, lost, err
	}
	a.clockHeld, a.clockMoved, a.clockHeldStart = k, k, start
	return true, lost, nil
}

// beginClock moves the run's clock for test: to at, or, for a test whose
// recording lists a mock.real destination (real), onto real time. A build
// that freezes the run and cannot do the latter fails the begin
// (SetTestBegins refuses such a recording first); one that freezes nothing
// has no clock to move, and its REAL test keeps the real clock it is on.
func beginClock(ctx context.Context, c TestClock, test string, at time.Time, real bool) error {
	if !real {
		return c.BeginTestClock(ctx, test, at)
	}
	if rc, ok := c.(RealTestClock); ok {
		return rc.BeginRealTestClock(ctx, test)
	}
	if TimeFile() == "" {
		return nil
	}
	return ErrNoRealTestClock
}

// lostScope is a test that held the run's clock and whose worker exited
// (beginTestClock): its key, and its worker's start time, which tells that
// worker's open tests from those of a new process on its pid.
type lostScope struct {
	key   scopeKey
	start string
}

// procStartTime is process pid's start time (/proc/<pid>/stat field 22), so a
// pid another process took since is told apart; "" when the agent cannot see
// a live process there: pid 0 (a harness that sends none) or 1, a process
// that has exited (a zombie too), a kernel thread (a pid from another pid
// namespace can name one), or not Linux. The scope API's pid is the worker's
// in the agent's pid namespace: keploy's container flow puts the app in the
// agent's.
func procStartTime(pid int) string {
	start, _ := procStatStart(pid)
	return start
}

// readProcStat reads /proc/<pid>/stat; a variable so a test can fail it.
var readProcStat = func(pid int) ([]byte, error) {
	return os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
}

// procStatStart is procStartTime, with the error that kept the agent from reading
// the process's stat.
func procStatStart(pid int) (string, error) {
	if pid <= 1 {
		return "", nil
	}
	b, err := readProcStat(pid)
	if err != nil {
		return "", err
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 {
		s = s[i+1:]
	}
	f := strings.Fields(s)
	if len(f) < 20 {
		return "", nil
	}
	// f[0] is the state, f[6] the flags (field 9); PF_KTHREAD is 0x00200000.
	if f[0] == "Z" || f[0] == "X" || f[0] == "x" {
		return "", nil
	}
	if flags, err := strconv.ParseUint(f[6], 10, 64); err != nil || flags&0x00200000 != 0 {
		return "", nil
	}
	return f[19], nil
}

// workerGone reports whether the worker at pid, whose start time was start at
// its test's begin, is gone: it exited (a zombie too), or its pid is another
// process's now. A stat the agent could not read for any other reason (out of
// file descriptors on a loaded machine, a denied read) tells nothing, and the
// test of a worker that may be alive is never failed for it.
func workerGone(pid int, start string) bool {
	now, err := procStatStart(pid)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
	}
	return now != start
}

// releaseTestClock moves the run's clock back to its start time when a move
// is in force, for a new session, and frees it: one section of the clock's
// lock, so a late begin of the previous session cannot slip in between. A
// clock that cannot move back stays marked moved, so the next try moves it
// again.
func (a *Agent) releaseTestClock(ctx context.Context) error {
	c, ok := testClock()
	a.clockMu.Lock()
	defer a.clockMu.Unlock()
	a.clockHeld, a.clockHeldStart = scopeKey{}, ""
	moved := a.clockMoved
	if moved.name == "" || !ok {
		a.clockMoved = scopeKey{}
		return nil
	}
	if err := c.EndTestClock(ctx, moved.name); err != nil {
		a.logger.Error("keploy could not move the run's clock back to its start time", zap.String("test", moved.name), zap.Error(err))
		return err
	}
	a.clockMoved = scopeKey{}
	return nil
}

// endTestClock moves the run's clock back to its start time when test (of
// worker pid) holds it, or its move is still in force after an end that
// could not move it back (a harness that retries the end); any other end (a
// suite, a subtest, a test whose begin failed) leaves it. The test gives the
// clock up either way; a move back that fails leaves the clock marked moved,
// for a retried end, the next begin (an absolute move) or the next session's
// release to undo.
func (a *Agent) endTestClock(ctx context.Context, test string, pid int) error {
	c, ok := testClock()
	if !ok {
		return nil
	}
	k := scopeKey{pid: uint32(pid), name: test}
	a.clockMu.Lock()
	defer a.clockMu.Unlock()
	if a.clockHeld != k && a.clockMoved != k {
		return nil
	}
	if a.clockHeld == k {
		a.clockHeld, a.clockHeldStart = scopeKey{}, ""
	}
	if err := c.EndTestClock(ctx, test); err != nil {
		a.logger.Error("keploy could not move the run's clock back after the test", zap.String("test", test), zap.Error(err))
		return err
	}
	a.clockMoved = scopeKey{}
	return nil
}
