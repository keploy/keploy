package replay

import "go.keploy.io/server/v3/pkg/models"

// noteComparedShare keeps the share of a set's final report (noteSetEnd), by
// set: a set run again (a flaky check, a retry) counts once, as its last run.
// A set that set nothing aside (a nil share) replaces what an earlier run of
// it noted.
func (r *Replayer) noteComparedShare(testSetID string, share *models.ComparedShare) {
	r.quarantinedMu.Lock()
	defer r.quarantinedMu.Unlock()
	if r.comparedShares == nil {
		r.comparedShares = map[string]*models.ComparedShare{}
	}
	r.comparedShares[testSetID] = share
}

// runComparedShare is the run's share, for its summary: its totals, and the
// reasons its sets set their tests aside for (noteComparedShare). Nil when it
// set none aside.
func (r *Replayer) runComparedShare(compared, total, setAside int) *models.ComparedShare {
	counts := map[string]int{}
	r.quarantinedMu.Lock()
	for _, s := range r.comparedShares {
		for reason, n := range s.ReasonCounts() {
			counts[reason] += n
		}
	}
	r.quarantinedMu.Unlock()
	return models.NewComparedShare(compared, total, setAside, counts)
}

// comparedShareLine is the line that ends the run's summary when it set any
// test aside, so the last thing it prints leads with how much of the suite it
// compared: "6 of 307 compared; 301 set aside: 301 keploy/unmocked_call".
// A run whose status says PASSED compared only that share.
func comparedShareLine(share *models.ComparedShare) string {
	if share == nil {
		return ""
	}
	return share.Line() + "\n\n"
}
