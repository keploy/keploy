package proxy

import (
	"slices"
	"testing"
	"time"
)

// everyTouch is onceTouch's series with every on it (SetPoolEvery between
// SetPoolOnce and SetPoolSeries, as the agent hands them), its rounds read
// on the clock the returned func moves.
func everyTouch(t *testing.T, every time.Duration) (*MockManager, func(time.Duration)) {
	t.Helper()
	mm := onceTouch(t, true)
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	mm.exact.outside.now = func() time.Time { return now }
	mm.SetPoolEvery(map[string]time.Duration{onceID: every})
	mm.SetPoolSeries(map[string][]string{onceID: {"p1", "p2", "p3"}})
	return mm, func(d time.Duration) { now = now.Add(d) }
}

// An every series answers each of its calls once in a round. A repeat
// sooner than the interval after the round opened misses, in a window and
// between tests, its last-placed recording's included; one that comes the
// interval later opens the next round, where each call answers once again,
// in a later window too; and the round after that is read from the second
// round's opening, not the first's.
func TestAnEverySeriesAnswersEachCallOnceARound(t *testing.T) {
	mm, after := everyTouch(t, 5*time.Minute)
	mm.ServeExactTest("TestA", nil)
	got := []string{callURL(t, mm, onceByID, false), callURL(t, mm, onceRead, false), callURL(t, mm, onceWrite, false)}
	if want := []string{"p1", "p2", "p3"}; !slices.Equal(got, want) {
		t.Fatalf("the first round answered %v, want %v", got, want)
	}
	after(4 * time.Minute)
	if got := callURL(t, mm, onceWrite, false); got != "a miss" {
		t.Fatalf("the touch again 4m into a 5m round was answered %q, want a miss", got)
	}
	mm.ServeExactPool("k", nil)
	if got := callURL(t, mm, onceByID, true); got != "a miss" {
		t.Fatalf("the first call again between tests 4m into the round was answered %q, want a miss", got)
	}
	after(time.Minute)
	mm.ServeExactTest("TestB", nil)
	got = []string{callURL(t, mm, onceByID, false), callURL(t, mm, onceRead, false), callURL(t, mm, onceWrite, false), callURL(t, mm, onceWrite, false)}
	if want := []string{"p1", "p2", "p3", "a miss"}; !slices.Equal(got, want) {
		t.Fatalf("5m after the first round the series answered %v, want %v", got, want)
	}
	after(4 * time.Minute)
	if got := callURL(t, mm, onceByID, false); got != "a miss" {
		t.Fatalf("9m after the first round, 4m into the second, the first call was answered %q, want a miss", got)
	}
	if got := mm.SeriesRepeats(); got != nil {
		t.Fatalf("an every series counted repeats %v, want none", got)
	}
}

// A round opens on a call that comes everyLead sooner than the interval,
// the time an application's call takes from its clock read to the agent,
// and no sooner. Between tests, a call of a recording not yet answered in
// the round answers in it, whichever opened the round.
func TestAnEverySeriesRoundOpensNoSoonerThanTheLead(t *testing.T) {
	mm, after := everyTouch(t, 5*time.Minute)
	mm.ServeExactPool("k", nil)
	if got := callURL(t, mm, onceWrite, true); got != "p3" {
		t.Fatalf("the write first between tests was answered %q, want p3", got)
	}
	after(5*time.Minute - everyLead - time.Millisecond)
	if got := callURL(t, mm, onceWrite, true); got != "a miss" {
		t.Fatalf("the write a millisecond before the lead was answered %q, want a miss", got)
	}
	if got := callURL(t, mm, onceByID, true); got != "p1" {
		t.Fatalf("p1, not yet answered in the round, was answered %q, want p1", got)
	}
	after(time.Millisecond)
	if got := callURL(t, mm, onceWrite, true); got != "p3" {
		t.Fatalf("the write at the interval less the lead was answered %q, want p3 (round 2)", got)
	}
	if got := callURL(t, mm, onceByID, true); got != "p1" {
		t.Fatalf("p1 in round 2 was answered %q, want p1", got)
	}
	if got := callURL(t, mm, onceByID, true); got != "a miss" {
		t.Fatalf("p1 twice in round 2 was answered %q, want a miss", got)
	}
}

// SetPoolEvery marks only once series, with a positive interval; a table
// with none leaves none, and a series it does not mark answers once in the
// run however long it runs.
func TestSetPoolEveryMarksOnlyOnceSeries(t *testing.T) {
	mm := onceTouch(t, false)
	mm.SetPoolEvery(map[string]time.Duration{onceID: time.Minute, "": time.Minute, "zero": 0})
	mm.SetPoolSeries(map[string][]string{onceID: {"p1", "p2", "p3"}})
	if m := mm.exact.poolSeries.Load().member("p1"); m.every != 0 {
		t.Fatalf("a series not once got every %v, want none", m.every)
	}
	if set := *mm.exact.poolEvery.Load(); len(set) != 1 || set[onceID] != time.Minute {
		t.Fatalf("the every set is %v, want only %s at 1m", set, onceID)
	}
	mm, after := everyTouch(t, time.Minute)
	mm.SetPoolEvery(nil)
	mm.SetPoolSeries(map[string][]string{onceID: {"p1", "p2", "p3"}})
	mm.ServeExactTest("TestA", nil)
	if got := callURL(t, mm, onceByID, false); got != "p1" {
		t.Fatalf("the first call was answered %q, want p1", got)
	}
	after(time.Hour)
	if got := callURL(t, mm, onceByID, false); got != "a miss" {
		t.Fatalf("with the every set emptied the first call again an hour later was answered %q, want a miss", got)
	}
}

// A test process that claims its key again starts its every series'
// rounds afresh: its first call answers at once, and the next round is
// read from that call.
func TestRestartingAKeyStartsItsEverySeriesRoundsAfresh(t *testing.T) {
	mm, after := everyTouch(t, 5*time.Minute)
	mm.ServeExactPool("k", nil)
	if got := callURL(t, mm, onceByID, true); got != "p1" {
		t.Fatalf("the first call was answered %q, want p1", got)
	}
	after(4 * time.Minute)
	mm.RestartExactKey("k", []string{"p1", "p2", "p3"})
	mm.ServeExactPool("k", nil)
	if got := callURL(t, mm, onceByID, true); got != "p1" {
		t.Fatalf("the restarted process's first call was answered %q, want p1", got)
	}
	after(2 * time.Minute)
	if got := callURL(t, mm, onceByID, true); got != "a miss" {
		t.Fatalf("6m after the first round but 2m after the restart the call was answered %q, want a miss", got)
	}
}
