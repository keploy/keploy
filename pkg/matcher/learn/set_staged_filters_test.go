package learn

import (
	"testing"
)

// A staged set's staging (WarmSet) builds the filter of each recording's
// words (stageFilter), so none of its calls builds one: the set's first
// call that reads a word's places (decider.places) asks every recording
// before it its filter, and built each of them while its client waited
// (api-server's mongo round: up to 84 ms of one call). What the calls
// decide, scan, look up and read is what they do with no staging: the
// staging only builds, ahead of them, what the first call would have.
// Counted, not timed, over 300 and 3,000 recordings.
func TestASetsStagingBuildsEachRecordingsWordFilterSoItsCallsBuildNone(t *testing.T) {
	for _, n := range []int{300, 3000} {
		got, builds, scans, lookups, asks, _ := costSetStaged(n, false)
		sGot, sBuilds, sScans, sLookups, sAsks, use := costSetStaged(n, true)
		t.Logf("%d recordings: staging %+v; filters built by the calls %v staged, %v not", n, use, sBuilds, builds)
		if use.Staged != n || use.Filters != n || use.FiltersPast != 0 {
			t.Fatalf("over %d recordings the staging read %d and built %d filters (%d past the budget); want all %d, none past", n, use.Staged, use.Filters, use.FiltersPast, n)
		}
		if builds != [2]int{n, 0} {
			t.Fatalf("over %d recordings the calls of the set with no staging built %v filters; want each recording's once, in the first test: [%d 0]", n, builds, n)
		}
		if sBuilds != [2]int{0, 0} {
			t.Errorf("over %d recordings the calls of the staged set built %v filters; want none: the staging built every one", n, sBuilds)
		}
		if sGot != got {
			t.Errorf("over %d recordings the staged set's tests decide %q; the set with no staging decides %q", n, sGot, got)
		}
		if sScans != scans || sLookups != lookups || sAsks != asks {
			t.Errorf("over %d recordings the staged set's calls scanned %v, looked up %v and read %v places; with no staging %v, %v and %v", n, sScans, sLookups, sAsks, scans, lookups, asks)
		}
	}
}

// The staging builds the recordings' filters (stageFilters) only once its
// first pass read every recording within the set's budget, and then only
// with the budget's spare: where the first pass filled the budget
// (SetUse.HeldFull), it builds none, and the set keeps what it kept with no
// filter staged; where a filter does not fit the spare, it stops there
// (SetUse.FiltersPast), and the calls that ask the rest a word build them,
// or scan them, as with no staging. Either way the staged set's tests
// decide what the set with no staging decides at that budget. Over budgets
// from one the first pass fills to one it leaves a little spare in.
func TestASetsStagingLeavesTheFiltersPastItsBudgetToItsCalls(t *testing.T) {
	defer func(b int64) { learnBudget = b }(learnBudget)
	const n = 300
	filled, past := false, false
	for _, b := range []int64{8 << 10, 64 << 10, 512 << 10, 1 << 20, 3 << 19} {
		learnBudget = b
		got, _, _, _, _, _ := costSetStaged(n, false)
		sGot, sBuilds, _, _, _, use := costSetStaged(n, true)
		t.Logf("budget %d: staging %+v; filters built by the staged set's calls %v", b, use, sBuilds)
		if sGot != got {
			t.Errorf("at a budget of %d bytes the staged set's tests decide %q; the set with no staging decides %q", b, sGot, got)
		}
		switch {
		case use.HeldFull && (use.Filters != 0 || use.FiltersPast != 0):
			t.Errorf("at a budget of %d bytes the first pass filled it (read %d of %d), and the staging still built %d filters (%d past): want none", b, use.Staged, n, use.Filters, use.FiltersPast)
		case use.HeldFull:
			filled = true
		case use.FiltersPast > 0:
			past = true
			if use.Filters+use.FiltersPast >= n {
				t.Errorf("at a budget of %d bytes the staging built %d filters and counted %d past: want it stopped at the first that did not fit, short of all %d", b, use.Filters, use.FiltersPast, n)
			}
		}
	}
	if !filled || !past {
		t.Fatalf("no budget had the first pass fill it (%v), or none left a filter past its spare (%v): the row reads neither fallback", filled, past)
	}
}
