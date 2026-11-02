package realdeps

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

func newTracker() *Tracker {
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	return &Tracker{now: func() time.Time { return at }}
}

func TestATestsRealRowsCountItsConnectionsAndFailedDials(t *testing.T) {
	tr := newTracker()
	// A pooled connection opened before the test, and reached outside it.
	tr.Dialed(Origin{}, 1, "db:5432", true)
	tr.Sent(Origin{}, 1, "db:5432") // outside every test: no row

	tr.BeginTest()
	tr.Sent(Origin{}, 1, "db:5432")
	tr.Sent(Origin{}, 1, "db:5432")
	tr.Dialed(Origin{}, 2, "db:5432", true)
	tr.Sent(Origin{}, 2, "db:5432")
	tr.Dialed(Origin{}, 3, "auth.example.com:443", false)
	tr.Dialed(Origin{}, 4, "auth.example.com:443", true)
	tr.Sent(Origin{}, 4, "auth.example.com:443")
	tr.H2(Origin{}, "auth.example.com:443")
	uses, inc := tr.EndTest()
	require.Empty(t, inc)
	require.Equal(t, []models.RealUse{
		{Dest: "auth.example.com:443", Conns: 1, Failed: 1, H2: true},
		{Dest: "db:5432", Conns: 2},
	}, uses)

	// The next test that uses the pooled connection has its own row.
	tr.BeginTest()
	tr.Sent(Origin{}, 1, "db:5432")
	uses, _ = tr.EndTest()
	require.Equal(t, []models.RealUse{{Dest: "db:5432", Conns: 1}}, uses)

	tr.BeginTest()
	uses, inc = tr.EndTest()
	require.Nil(t, uses)
	require.Nil(t, inc)

	run := tr.Run()
	require.Equal(t, []string{"auth.example.com:443", "db:5432"}, run.Reached)
	require.Equal(t, []string{"db:5432"}, run.Outside, "first reached while no test was open")
	require.Empty(t, run.Incomplete)
}

// Evidence of a connection keploy ended is the open test's; made while no
// test is open (before the first test, between tests, after the last) it touched
// no test, and is the run's: no test is charged for it.
func TestEvidenceIsTheOpenTestsOrTheRuns(t *testing.T) {
	tr := newTracker()
	tr.Incomplete(Origin{}, "10.0.0.1:443", "before the first test")
	tr.Incomplete(Origin{}, "10.0.0.1:443", "between tests")
	tr.BeginTest()
	tr.Incomplete(Origin{}, "10.0.0.1:443", "in the test")
	_, inc := tr.EndTest()
	require.Len(t, inc, 1, "only what was made while it was open")
	require.Equal(t, "in the test", inc[0].Why)
	require.Equal(t, models.CaptureIncompleteRealName, inc[0].Class)

	tr.Incomplete(Origin{}, "10.0.0.1:443", "after the last test")
	run := tr.Run()
	require.Len(t, run.Incomplete, 3)
	require.Equal(t, "before the first test", run.Incomplete[0].Why)
	require.Equal(t, "between tests", run.Incomplete[1].Why)
	require.Equal(t, "after the last test", run.Incomplete[2].Why)
	require.Empty(t, tr.Run().Incomplete, "a read takes it")
}

// A connection that stays undecided across tests N and N+1 is charged to
// both: it was undecided during each (design v14 §3.2 as amended for class
// e). A test that never had it undecided is not charged, and a read charges
// only what no test was charged.
func TestAConnectionUndecidedAcrossTestsIsChargedToEach(t *testing.T) {
	tr := newTracker()
	tr.BeginTest() // N
	done := tr.Pending(Origin{}, 7, "10.0.0.1:3306", []string{"db.example.com"})
	_, inc := tr.EndTest()
	require.Len(t, inc, 1)
	require.Equal(t, "10.0.0.1:3306", inc[0].Destination)
	require.Contains(t, inc[0].Why, "db.example.com")
	require.Contains(t, inc[0].Why, "the test ended")

	tr.BeginTest() // N+1
	_, inc = tr.EndTest()
	require.Len(t, inc, 1, "still undecided during N+1: charged to it too")
	require.Empty(t, tr.Run().Incomplete, "charged to tests: none of it the run's")

	// Closed unnamed between tests: every test it was undecided in was
	// charged already.
	done("its client closed it before naming it")
	tr.BeginTest() // never touched it
	_, inc = tr.EndTest()
	require.Empty(t, inc, "a test that never had it undecided is not charged")
	require.Empty(t, tr.Run().Incomplete)

	// Undecided at N's end and closed unnamed during N+1: charged to both.
	tr.BeginTest()
	done = tr.Pending(Origin{}, 8, "10.0.0.2:3306", nil)
	_, inc = tr.EndTest()
	require.Len(t, inc, 1)
	tr.BeginTest()
	done("unreadable")
	_, inc = tr.EndTest()
	require.Len(t, inc, 1)
	require.Equal(t, "unreadable", inc[0].Why)

	// Named during a test: nothing is charged.
	tr.BeginTest()
	done = tr.Pending(Origin{}, 9, "10.0.0.3:3306", nil)
	done("")
	_, inc = tr.EndTest()
	require.Empty(t, inc)

	// Opened and closed unnamed while no test was open: it touched no test,
	// and is the run's.
	tr.Pending(Origin{}, 10, "10.0.0.4:3306", nil)("unreadable")
	tr.BeginTest()
	_, inc = tr.EndTest()
	require.Empty(t, inc)

	// One undecided when the results are read, never during a test, is the
	// run's too.
	tr.Pending(Origin{}, 11, "10.0.0.5:3306", []string{"db.example.com"})
	run := tr.Run()
	require.Len(t, run.Incomplete, 2)
	require.Equal(t, "10.0.0.4:3306", run.Incomplete[0].Destination)
	require.Contains(t, run.Incomplete[1].Why, "results were read")
}

func TestAnInterruptedTestsEvidenceIsTheRuns(t *testing.T) {
	tr := newTracker()
	tr.BeginTest()
	tr.Dialed(Origin{}, 1, "db:5432", true)
	tr.Sent(Origin{}, 1, "db:5432")
	tr.Incomplete(Origin{}, "10.0.0.1:443", "in the interrupted test")
	tr.Interrupt()
	tr.Interrupt() // nothing open: nothing to do
	run := tr.Run()
	require.Len(t, run.Incomplete, 1)
	require.Equal(t, []string{"db:5432"}, run.Reached)
	require.Empty(t, run.Outside, "reached while a test was open")
}

func TestResetStartsARun(t *testing.T) {
	tr := newTracker()
	tr.Dialed(Origin{}, 1, "db:5432", true)
	tr.Incomplete(Origin{}, "x:1", "old")
	tr.Pending(Origin{}, 2, "10.0.0.1:443", []string{"a"})
	tr.BeginTest()
	_, _ = tr.EndTest() // charges the pending connection
	tr.Reset()
	run := tr.Run()
	require.Empty(t, run.Reached)
	require.Len(t, run.Incomplete, 1, "a connection still pending belongs to the new run, charged afresh")
}

// A connection a test's end charged as undecided and closed unnamed after it,
// with no test open, is not charged again: the test it was undecided in holds
// it.
func TestAPendingConnectionClosedAfterItsTestIsNotChargedAgain(t *testing.T) {
	tr := newTracker()
	tr.BeginTest()
	done := tr.Pending(Origin{}, 1, "10.0.0.1:443", []string{"a"})
	_, inc := tr.EndTest()
	require.Len(t, inc, 1)
	done("unreadable")
	require.Empty(t, tr.Run().Incomplete)
}

// keploy record names a test case's REAL destinations by its window: a use
// (a send, or a dial that failed) from its request to its response, both
// included. A wall clock that steps back keeps the uses in order.
func TestUsedDuringNamesATestCasesWindow(t *testing.T) {
	base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	at := base
	tr := &Tracker{now: func() time.Time { return at }}
	tr.KeepUses(true)
	use := func(d time.Duration, dest string, sent bool) {
		at = base.Add(d)
		if sent {
			tr.Sent(Origin{}, 1, dest)
			return
		}
		tr.Dialed(Origin{}, 2, dest, false)
	}
	use(10*time.Millisecond, "db:5432", true)
	use(20*time.Millisecond, "auth:443", false)
	use(40*time.Millisecond, "db:5432", true)
	use(5*time.Millisecond, "cache:6379", true) // the wall clock stepped back

	w := func(from, to time.Duration) []string { return tr.UsedDuring(base.Add(from), base.Add(to)) }
	require.Equal(t, []string{"db:5432"}, w(6*time.Millisecond, 10*time.Millisecond), "a use at the window's end is in it")
	require.Equal(t, []string{"cache:6379", "db:5432"}, w(0, 10*time.Millisecond))
	require.Equal(t, []string{"auth:443", "db:5432"}, w(10*time.Millisecond, 20*time.Millisecond))
	require.Nil(t, w(21*time.Millisecond, 39*time.Millisecond))
	require.Equal(t, []string{"db:5432"}, w(40*time.Millisecond, time.Second))
	require.Equal(t, []string{"cache:6379"}, w(time.Millisecond, 6*time.Millisecond))
	require.Nil(t, tr.UsedDuring(time.Time{}, base), "a window with no start names nothing")
	require.Nil(t, tr.UsedDuring(base.Add(time.Second), base), "nor one that ends before it starts")

	tr.Reset()
	require.Nil(t, w(0, time.Second), "a new run starts with no uses")
}

// A replay keeps no use points: nothing names its test cases by time.
func TestUsesAreKeptOnlyWhenAsked(t *testing.T) {
	tr := newTracker()
	tr.Sent(Origin{}, 1, "db:5432")
	require.Nil(t, tr.UsedDuring(time.Time{}.Add(1), time.Now().Add(time.Hour)))
	tr.KeepUses(true)
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	tr.Sent(Origin{}, 1, "db:5432")
	require.Equal(t, []string{"db:5432"}, tr.UsedDuring(at, at))
	tr.KeepUses(true) // a reconnect's stream opens before the last closes
	tr.KeepUses(false)
	require.Equal(t, []string{"db:5432"}, tr.UsedDuring(at, at), "kept while any stream is open")
	tr.KeepUses(false)
	require.Nil(t, tr.UsedDuring(at, at), "the kept uses go with the last")
	tr.KeepUses(false)
	tr.Sent(Origin{}, 1, "db:5432")
	require.Nil(t, tr.UsedDuring(at, at), "an extra end keeps nothing")
}

// A pooled connection dialled before the last read reaches its destination
// again when the destination answers it, not when bytes are written to it:
// each read says what was reached since the one before.
func TestAPooledConnectionReachesItsDestinationWhenItAnswers(t *testing.T) {
	tr := newTracker()
	tr.Dialed(Origin{}, 1, "db:5432", true)
	require.Equal(t, []string{"db:5432"}, tr.Run().Reached)
	require.Empty(t, tr.Run().Reached, "a read says what was reached since the last")
	tr.BeginTest()
	tr.Sent(Origin{}, 1, "db:5432")
	require.Empty(t, tr.Run().Reached, "bytes written into a connection whose destination died reach nothing")
	tr.Answered("db:5432")
	_, _ = tr.EndTest()
	require.Equal(t, []string{"db:5432"}, tr.Run().Reached)
}

// An h2 note that comes after a test boundary (the send was the last test's)
// makes no row for the test that began: its own sends say h2.
func TestAnH2NoteAfterATestBoundaryMakesNoRow(t *testing.T) {
	var tr Tracker
	tr.BeginTest()
	tr.Sent(Origin{}, 1, "auth.example.com:443")
	_, _ = tr.EndTest()
	tr.BeginTest()
	tr.H2(Origin{}, "auth.example.com:443")
	uses, _ := tr.EndTest()
	require.Empty(t, uses)
}

// A test whose window was left open (never ended) when the next begins:
// its evidence is the run's, never charged to the test that begins.
func TestATestLeftOpenGivesTheRunItsEvidence(t *testing.T) {
	tr := newTracker()
	tr.BeginTest()
	tr.Incomplete(Origin{}, "10.0.0.1:443", "in the test left open")
	require.Empty(t, tr.Run().Incomplete, "an open test's evidence is its own until it ends")
	tr.BeginTest()
	_, inc := tr.EndTest()
	require.Empty(t, inc, "the test that began never touched it")
	run := tr.Run()
	require.Len(t, run.Incomplete, 1)
	require.Equal(t, "in the test left open", run.Incomplete[0].Why)
}

// A connect that failed while keploy could not yet tell whether its
// destination is a mock.real container (the index still placing it) is a
// finished event of the test it failed in, and only of it: told REAL while
// that test is open, its REAL row counts the failure; that test ending first
// is charged with it as capture_incomplete; no other test ever is. A
// dependency's charges nothing. One that failed while no test was open, or
// whose test never ended, is the run's.
func TestAFailedConnectStillUndecidedIsChargedOnlyToItsTest(t *testing.T) {
	tr := newTracker()
	tr.BeginTest()
	tr.FailedUndecided(Origin{}, "127.0.0.1:6379")("container:redis", false)
	uses, inc := tr.EndTest()
	require.Equal(t, []models.RealUse{{Dest: "container:redis", Failed: 1}}, uses)
	require.Empty(t, inc)

	// Its test ends first: charged to it, never to the next.
	tr.BeginTest()
	resolve := tr.FailedUndecided(Origin{}, "127.0.0.1:6379")
	_, inc = tr.EndTest()
	require.Len(t, inc, 1, "undecided when its test ended")
	require.Contains(t, inc[0].Why, "not yet placed when the test ended")
	tr.BeginTest()
	_, inc = tr.EndTest()
	require.Empty(t, inc, "a test that began after it failed is never charged")
	tr.BeginTest()
	resolve("container:redis", false)
	uses, inc = tr.EndTest()
	require.Empty(t, uses, "told REAL during a later test: not its")
	require.Empty(t, inc)

	tr.BeginTest()
	tr.FailedUndecided(Origin{}, "10.0.0.9:5432")("", false)
	uses, inc = tr.EndTest()
	require.Empty(t, uses, "a dependency's failed connect")
	require.Empty(t, inc)

	// Told unresolved while its test is open: that test's.
	tr.BeginTest()
	tr.FailedUndecided(Origin{}, "10.0.0.9:6379")("", true)
	_, inc = tr.EndTest()
	require.Len(t, inc, 1)

	// Failed between tests: the next test ending before the tell is not
	// charged; told unresolved, or found untold by a read, it is the run's.
	between := tr.FailedUndecided(Origin{}, "10.0.0.8:6379")
	tr.BeginTest()
	_, inc = tr.EndTest()
	require.Empty(t, inc, "a connect that failed between tests is no test's")
	between("", true)
	tr.FailedUndecided(Origin{}, "10.0.0.7:6379")
	run := tr.Run()
	require.Len(t, run.Incomplete, 2)
	require.Equal(t, "10.0.0.8:6379", run.Incomplete[0].Destination)
	require.Contains(t, run.Incomplete[1].Why, "results were read")

	// Its test never ended: told REAL during the next test, it does not
	// count there.
	tr.BeginTest()
	left := tr.FailedUndecided(Origin{}, "127.0.0.1:6379")
	tr.BeginTest() // the last was left open
	left("container:redis", false)
	uses, inc = tr.EndTest()
	require.Empty(t, uses, "a test that never ended keeps its failure off the next test's row")
	require.Empty(t, inc)

	// A new run drops the last run's failed connects.
	old := tr.FailedUndecided(Origin{}, "127.0.0.1:6379")
	tr.Reset()
	tr.BeginTest()
	old("", true)
	_, inc = tr.EndTest()
	require.Empty(t, inc)
	require.Empty(t, tr.Run().Incomplete)
}

// worker is a connection's origin in test worker n's keploy instance.
func worker(n uint32) Origin { return Origin{Instance: n, Pid: 1000 + n} }

// Two workers' tests open at once: each one's REAL row holds what its own
// worker's connections did, its evidence what was made on them, and a worker
// with no test open uses nothing of the other's.
func TestEachWorkersTestHoldsItsOwnWorkersRealUses(t *testing.T) {
	tr := newTracker()
	tr.BeginTestOf(1)
	tr.BeginTestOf(2)
	tr.Sent(worker(1), 1, "db:5432")
	tr.Dialed(worker(1), 2, "auth:443", false)
	tr.Sent(worker(2), 3, "cache:6379")
	tr.H2(worker(2), "cache:6379")
	tr.H2(worker(1), "cache:6379") // worker 1 sent cache nothing: no row to mark
	tr.Incomplete(worker(2), "10.0.0.7:443", "keploy could not read its name")
	tr.Sent(worker(3), 4, "db:5432") // worker 3 has no test open
	tr.Incomplete(worker(3), "10.0.0.8:443", "worker 3's, between its tests")

	two := tr.EndTestOf(2)
	require.Equal(t, []models.RealUse{{Dest: "cache:6379", Conns: 1, H2: true}}, two.Uses)
	require.Len(t, two.Incomplete, 1)
	require.Equal(t, "10.0.0.7:443", two.Incomplete[0].Destination)
	require.Empty(t, two.Unattributed)
	// Worker 2's next test begins with nothing of its last, nor of worker 1's.
	tr.BeginTestOf(2)
	tr.Sent(worker(2), 3, "cache:6379")
	one := tr.EndTestOf(1)
	require.Equal(t, []models.RealUse{{Dest: "auth:443", Failed: 1}, {Dest: "db:5432", Conns: 1}}, one.Uses)
	require.Empty(t, one.Incomplete)
	require.Empty(t, one.Unattributed)
	require.Equal(t, []models.RealUse{{Dest: "cache:6379", Conns: 1}}, tr.EndTestOf(2).Uses)
	run := tr.Run()
	require.Len(t, run.Incomplete, 1, "what worker 3 made with no test of its own open is the run's")
	require.Equal(t, "10.0.0.8:443", run.Incomplete[0].Destination)
}

// A connection from a process in no worker's instance, while only workers'
// tests are open, could be any of theirs: its REAL use is in no test's row
// and charged to each test open then as unattributed, naming the latest and
// its process, and its evidence is each one's. In a run of one test process
// (instance 0's test open) the same connection is that test's own.
func TestARealUseFromNoWorkerIsChargedToEveryOpenTestAsUnattributed(t *testing.T) {
	tr := newTracker()
	tr.BeginTestOf(1)
	tr.BeginTestOf(2)
	tr.Sent(Origin{Pid: 77}, 1, "db:5432")
	tr.Sent(Origin{Pid: 77}, 1, "db:5432") // the same connection again
	tr.Dialed(Origin{Pid: 78}, 2, "auth:443", false)
	tr.SentLive(Origin{Pid: 79}, 3, "app:8080")
	tr.Incomplete(Origin{Pid: 77}, "10.0.0.7:443", "keploy could not read its name")
	for _, inst := range []uint32{1, 2} {
		end := tr.EndTestOf(inst)
		require.Empty(t, end.Uses, "never a row of a test it guessed")
		require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossUnattributed, Count: 3,
			What: "the latest: a connection to mock.real destination app:8080, from process 79"}}, end.Unattributed)
		require.Len(t, end.Incomplete, 1, "its evidence is each open test's")
	}
	require.Empty(t, tr.Run().Incomplete)

	// With no test open it is the run's, and charges no later test.
	tr.Sent(Origin{}, 4, "db:5432")
	tr.BeginTestOf(1)
	require.Empty(t, tr.EndTestOf(1).Unattributed)

	// A run of one test process: instance 0's own.
	tr.BeginTest()
	tr.Sent(Origin{Pid: 77}, 5, "db:5432")
	uses, _ := tr.EndTest()
	require.Equal(t, []models.RealUse{{Dest: "db:5432", Conns: 1}}, uses)

	// A process keploy could not name is not named.
	tr.BeginTestOf(1)
	tr.Sent(Origin{}, 6, "db:5432")
	require.Equal(t, "the latest: a connection to mock.real destination db:5432", tr.EndTestOf(1).Unattributed[0].What)
}

// A connection whose name is still being read is charged at the end of its
// own worker's test, and, from no worker, at every test's end; never at
// another worker's. Closed unnamed, it is its worker's open test's.
func TestAPendingConnectionIsChargedToItsOwnWorkersTests(t *testing.T) {
	tr := newTracker()
	tr.BeginTestOf(1)
	tr.BeginTestOf(2)
	doneTwo := tr.Pending(worker(2), 1, "10.0.0.7:443", nil)
	doneNone := tr.Pending(Origin{}, 2, "10.0.0.8:443", nil)
	one := tr.EndTestOf(1)
	require.Len(t, one.Incomplete, 1, "worker 2's is not worker 1's")
	require.Equal(t, "10.0.0.8:443", one.Incomplete[0].Destination)
	tr.BeginTestOf(1)
	doneTwo("its client closed it unnamed")
	two := tr.EndTestOf(2)
	require.Len(t, two.Incomplete, 2)
	require.Equal(t, "10.0.0.7:443", two.Incomplete[0].Destination, "closed unnamed while its test was open")
	require.Equal(t, "its client closed it unnamed", two.Incomplete[0].Why)
	require.Equal(t, "10.0.0.8:443", two.Incomplete[1].Destination, "undecided during it too")
	doneNone("its client closed it unnamed")
	one = tr.EndTestOf(1)
	require.Len(t, one.Incomplete, 1, "closed unnamed while only worker 1's test was open")
	require.Equal(t, "10.0.0.8:443", one.Incomplete[0].Destination)
}

// A connect that failed while keploy could not tell what it reached is its
// own worker's test's (or, from no worker, each open test's): told REAL, a
// failure in that test's row (or an unattributed use of each); told
// unresolved, its evidence; and a test that ends first is charged with it
// once.
func TestAFailedConnectStillUndecidedIsItsOwnWorkersTests(t *testing.T) {
	tr := newTracker()
	tr.BeginTestOf(1)
	tr.BeginTestOf(2)
	tr.FailedUndecided(worker(2), "127.0.0.1:6379")("container:redis", false)
	unattributed := tr.FailedUndecided(Origin{Pid: 9}, "127.0.0.1:6380")
	unresolved := tr.FailedUndecided(Origin{Pid: 9}, "127.0.0.1:6381")
	one := tr.EndTestOf(1)
	require.Empty(t, one.Uses, "worker 2's failure is not worker 1's")
	require.Len(t, one.Incomplete, 2, "both of no worker's: undecided when it ended")
	tr.BeginTestOf(1)
	unattributed("container:redis", false)
	unresolved("", true)
	two := tr.EndTestOf(2)
	require.Equal(t, []models.RealUse{{Dest: "container:redis", Failed: 1}}, two.Uses)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossUnattributed, Count: 1,
		What: "the latest: a connection to mock.real destination container:redis, from process 9"}}, two.Unattributed)
	require.Len(t, two.Incomplete, 1, "told unresolved while it was open")
	require.Contains(t, two.Incomplete[0].Why, "could not place")
	again := tr.EndTestOf(1)
	require.Empty(t, again.Unattributed, "it began after the connect failed")
	require.Empty(t, again.Incomplete)
	require.Empty(t, tr.Run().Incomplete, "each was charged once already")
}

// A worker's test left open is dropped by that worker's next begin, giving
// the run its evidence, and another worker's open test stays; an interrupt
// drops every one. A destination is reached outside tests only when none is
// open.
func TestAWorkersTestLeftOpenIsItsOwnToDrop(t *testing.T) {
	tr := newTracker()
	tr.BeginTestOf(1)
	tr.BeginTestOf(2)
	tr.Incomplete(worker(1), "10.0.0.1:443", "worker 1's")
	tr.Sent(worker(2), 1, "db:5432")
	tr.Dialed(worker(2), 1, "db:5432", true)
	tr.BeginTestOf(1)
	require.Equal(t, []models.RealUse{{Dest: "db:5432", Conns: 1}}, tr.EndTestOf(2).Uses, "worker 1's begin keeps worker 2's test")
	run := tr.Run()
	require.Len(t, run.Incomplete, 1, "the dropped test's evidence is the run's")
	require.Equal(t, []string{"db:5432"}, run.Reached)
	require.Empty(t, run.Outside, "worker 1's test was open")
	tr.BeginTestOf(2)
	tr.Incomplete(worker(1), "10.0.0.2:443", "worker 1's")
	tr.Incomplete(worker(2), "10.0.0.3:443", "worker 2's")
	tr.Interrupt()
	require.Empty(t, tr.EndTestOf(1).Incomplete)
	require.Empty(t, tr.EndTestOf(2).Incomplete)
	tr.Dialed(worker(2), 2, "cache:6379", true)
	run = tr.Run()
	require.Len(t, run.Incomplete, 2, "an interrupt gives the run every open test's")
	require.Equal(t, []string{"cache:6379"}, run.Outside, "reached with no test open")
}
