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
	tr.Dialed(1, "db:5432", true)
	tr.Sent(1, "db:5432") // outside every test: no row

	tr.BeginTest()
	tr.Sent(1, "db:5432")
	tr.Sent(1, "db:5432")
	tr.Dialed(2, "db:5432", true)
	tr.Sent(2, "db:5432")
	tr.Dialed(3, "auth.example.com:443", false)
	tr.Dialed(4, "auth.example.com:443", true)
	tr.Sent(4, "auth.example.com:443")
	tr.H2("auth.example.com:443")
	uses, inc := tr.EndTest()
	require.Empty(t, inc)
	require.Equal(t, []models.RealUse{
		{Dest: "auth.example.com:443", Conns: 1, Failed: 1, H2: true},
		{Dest: "db:5432", Conns: 2},
	}, uses)

	// The next test that uses the pooled connection has its own row.
	tr.BeginTest()
	tr.Sent(1, "db:5432")
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

func TestEvidenceIsChargedLikeAMiss(t *testing.T) {
	tr := newTracker()
	tr.Incomplete("10.0.0.1:443", "before the claim")
	tr.Claimed() // the run's, not the first test's
	tr.Incomplete("10.0.0.1:443", "between tests")
	tr.BeginTest()
	tr.Incomplete("10.0.0.1:443", "in the test")
	_, inc := tr.EndTest()
	require.Len(t, inc, 2, "the evidence made since the last end, and in the test")
	require.Equal(t, "between tests", inc[0].Why)
	require.Equal(t, models.CaptureIncompleteRealName, inc[1].Class)

	tr.Incomplete("10.0.0.1:443", "after the last test")
	run := tr.Run()
	require.Len(t, run.Incomplete, 2)
	require.Equal(t, "before the claim", run.Incomplete[0].Why)
	require.Equal(t, "after the last test", run.Incomplete[1].Why)
	require.Empty(t, tr.Run().Incomplete, "a read takes it")
}

func TestAConnectionStillUndecidedIsChargedOnce(t *testing.T) {
	tr := newTracker()
	done := tr.Pending(7, "10.0.0.1:3306", []string{"db.example.com"})
	tr.BeginTest()
	_, inc := tr.EndTest()
	require.Len(t, inc, 1)
	require.Equal(t, "10.0.0.1:3306", inc[0].Destination)
	require.Contains(t, inc[0].Why, "db.example.com")
	require.Contains(t, inc[0].Why, "the test ended")

	tr.BeginTest()
	_, inc = tr.EndTest()
	require.Empty(t, inc, "charged once")
	require.Empty(t, tr.Run().Incomplete)

	// Decided at last: nothing more to charge.
	done()
	tr.BeginTest()
	_, inc = tr.EndTest()
	require.Empty(t, inc)

	// One undecided when the results are read is the run's.
	tr.Pending(8, "10.0.0.2:3306", []string{"db.example.com"})
	run := tr.Run()
	require.Len(t, run.Incomplete, 1)
	require.Contains(t, run.Incomplete[0].Why, "results were read")
}

func TestAnInterruptedTestsEvidenceIsTheRuns(t *testing.T) {
	tr := newTracker()
	tr.BeginTest()
	tr.Dialed(1, "db:5432", true)
	tr.Sent(1, "db:5432")
	tr.Incomplete("10.0.0.1:443", "in the interrupted test")
	tr.Interrupt()
	tr.Interrupt() // nothing open: nothing to do
	run := tr.Run()
	require.Len(t, run.Incomplete, 1)
	require.Equal(t, []string{"db:5432"}, run.Reached)
	require.Empty(t, run.Outside, "reached while a test was open")
}

func TestResetStartsARun(t *testing.T) {
	tr := newTracker()
	tr.Dialed(1, "db:5432", true)
	tr.Incomplete("x:1", "old")
	tr.Pending(2, "10.0.0.1:443", []string{"a"})
	tr.BeginTest()
	_, _ = tr.EndTest() // charges the pending connection
	tr.Reset()
	run := tr.Run()
	require.Empty(t, run.Reached)
	require.Len(t, run.Incomplete, 1, "a connection still pending belongs to the new run, charged afresh")
}

// A connection a test's end charged as undecided is charged once: its own
// end says so.
func TestAPendingConnectionIsChargedOnce(t *testing.T) {
	tr := newTracker()
	done := tr.Pending(1, "10.0.0.1:443", []string{"a"})
	require.False(t, tr.Pending(2, "10.0.0.2:443", []string{"a"})(), "decided before any end: not charged")
	tr.BeginTest()
	_, inc := tr.EndTest()
	require.Len(t, inc, 1)
	require.True(t, done(), "charged at the test's end")
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
			tr.Sent(1, dest)
			return
		}
		tr.Dialed(2, dest, false)
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
	tr.Sent(1, "db:5432")
	require.Nil(t, tr.UsedDuring(time.Time{}.Add(1), time.Now().Add(time.Hour)))
	tr.KeepUses(true)
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	tr.Sent(1, "db:5432")
	require.Equal(t, []string{"db:5432"}, tr.UsedDuring(at, at))
	tr.KeepUses(true) // a reconnect's stream opens before the last closes
	tr.KeepUses(false)
	require.Equal(t, []string{"db:5432"}, tr.UsedDuring(at, at), "kept while any stream is open")
	tr.KeepUses(false)
	require.Nil(t, tr.UsedDuring(at, at), "the kept uses go with the last")
	tr.KeepUses(false)
	tr.Sent(1, "db:5432")
	require.Nil(t, tr.UsedDuring(at, at), "an extra end keeps nothing")
}

// A pooled connection dialled before the last read reaches its destination
// again when the destination answers it, not when bytes are written to it:
// each read says what was reached since the one before.
func TestAPooledConnectionReachesItsDestinationWhenItAnswers(t *testing.T) {
	tr := newTracker()
	tr.Dialed(1, "db:5432", true)
	require.Equal(t, []string{"db:5432"}, tr.Run().Reached)
	require.Empty(t, tr.Run().Reached, "a read says what was reached since the last")
	tr.BeginTest()
	tr.Sent(1, "db:5432")
	require.Empty(t, tr.Run().Reached, "bytes written into a connection whose destination died reach nothing")
	tr.Answered("db:5432")
	_, _ = tr.EndTest()
	require.Equal(t, []string{"db:5432"}, tr.Run().Reached)
}

// An h2 note that comes after a test boundary (the send was the last test's)
// makes no row for the test that began: its own sends say h2.
func TestAnH2NoteAfterATestBoundaryMakesNoRow(t *testing.T) {
	var tr Tracker
	tr.Claimed()
	tr.BeginTest()
	tr.Sent(1, "auth.example.com:443")
	_, _ = tr.EndTest()
	tr.BeginTest()
	tr.H2("auth.example.com:443")
	uses, _ := tr.EndTest()
	require.Empty(t, uses)
}
