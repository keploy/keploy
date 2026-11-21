package learn

import "testing"

// At the budget keploy gives a set by default (setBudget: a quarter of the
// agent's memory limit, budgetOf), a set's staging never makes its calls do
// more than they do with no staging: the staging reads the whole set with
// neither tier that lets nothing go full, and each test decides the same
// and scans texts, asks filters words and reads places no more often than
// the set with no staging. The row reads it for agents of 1, 4 and 16 GiB,
// over sets of 300 and 3,000 recordings.
func TestAtTheDefaultBudgetAStagedSetsCallsDoNoMoreThanWithNoStaging(t *testing.T) {
	defer func(b int64) { learnBudget = b }(learnBudget)
	for _, limit := range []int64{1 << 30, 4 << 30, 16 << 30} {
		learnBudget = budgetOf(limit)
		for _, n := range []int{300, 3000} {
			got, _, scans, lookups, asks := costSet(n)
			sGot, _, sScans, sLookups, sAsks, use := costSetStaged(n, true)
			t.Logf("limit %d GiB (budget %d), %d recordings: staging %+v; scans %v/%v, lookups %v/%v, places %v/%v (staged/none)",
				limit>>30, learnBudget, n, use, sScans, scans, sLookups, lookups, sAsks, asks)
			if use.Staged != n || use.HeldFull || use.FormsFull || use.Budget != learnBudget {
				t.Fatalf("at the budget of a %d GiB agent the staging of %d recordings read %d, held full %v, forms full %v, budget %d; want all read, neither full, budget %d",
					limit>>30, n, use.Staged, use.HeldFull, use.FormsFull, use.Budget, learnBudget)
			}
			if sGot != got {
				t.Errorf("at the budget of a %d GiB agent the staged set of %d recordings decides %q; with no staging %q", limit>>30, n, sGot, got)
			}
			for c := range got {
				if sScans[c] > scans[c] || sLookups[c] > lookups[c] || sAsks[c] > asks[c] {
					t.Errorf("at the budget of a %d GiB agent, test %d of the staged set of %d recordings scanned %d texts, asked filters %d words and read %d places; with no staging %d, %d and %d",
						limit>>30, c+1, n, sScans[c], sLookups[c], sAsks[c], scans[c], lookups[c], asks[c])
				}
			}
		}
	}
}
