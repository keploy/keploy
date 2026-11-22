package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A miss marked before the running set's staging is kept as an earlier set's
// call (missWindow.keptAsEarlierSet) only where at zero latency a read of
// its own set would have taken it: marked in a test's view, whenever; or
// marked with a round in a set the window noted the start of, before that
// set's last read of the window (the reads counted at the next set's start),
// in the pool view or a view installed while its mocks were staged alike. A
// miss of a parser that marks no round, one marked before the window's first
// set start, one marked after its set's last read, and one marked in the
// running set are not kept. The kept call names how many sets back it was
// made (missWindow.setsBack): 1 where the window keeps no start before its
// mark, or the mark's start is not before the running set.
func TestAnEarlierSetsCallIsKeptOnlyWhereItsSetsReadWouldHaveTakenIt(t *testing.T) {
	w := &missWindow{}
	// Set 1 staged at epoch 2 (no read yet), set 2 at epoch 4 (set 1 read
	// its window up to 3 times), set 3 at epoch 6 (5 reads by then).
	for _, s := range []setStart{{epoch: 2, reads: 0, set: 1}, {epoch: 4, reads: 3, set: 2}, {epoch: 6, reads: 5, set: 3}} {
		w.noteSetStart(s)
	}
	miss := func(mark models.RoundMark, epoch, reads uint64, test string) error {
		e := mockMiss("POST /orders")
		e.MismatchReport.Mark, e.MismatchReport.MarkEpoch, e.MismatchReport.MarkReads, e.MismatchReport.MarkedTest = mark, epoch, reads, test
		return e
	}
	settled, staging := models.RoundMarkedSettled, models.RoundMarkedStaging
	for _, c := range []struct {
		name string
		err  error
		kept bool
		back int
	}{
		{"in set 1's pool view before its last read", miss(settled, 2, 2, ""), true, 2},
		{"in set 1's pool view before its first read", miss(settled, 3, 0, ""), true, 2},
		{"in set 1's pool view after its last read", miss(settled, 3, 3, ""), false, 0},
		{"in a view set 1's staging installed, before its last read", miss(staging, 2, 1, ""), true, 2},
		{"in set 2's pool view before its last read", miss(settled, 5, 4, ""), true, 1},
		{"in set 2's pool view after its last read", miss(settled, 5, 5, ""), false, 0},
		{"in set 1's test's view after its last read", miss(settled, 3, 3, "test-1"), true, 2},
		{"in a test's view before the window's first set start", miss(settled, 1, 0, "test-0"), true, 1},
		{"in the pool view before the window's first set start", miss(settled, 1, 0, ""), false, 0},
		{"in the running set's pool view", miss(settled, 6, 5, ""), false, 0},
		{"by a parser that marks no round", miss(models.RoundUnmarked, 0, 0, ""), false, 0},
	} {
		if got := w.keptAsEarlierSet(c.err); got != c.kept {
			t.Errorf("a miss marked %s is kept %v; want %v", c.name, got, c.kept)
		}
		if c.kept {
			if got := w.setsBack(c.err, 3); got != c.back {
				t.Errorf("a miss marked %s, read in set 3, names %d set(s) back; want %d", c.name, got, c.back)
			}
		}
	}
	// A window whose first start is at epoch 0 (no staging before it) keeps
	// no miss of a parser that marks no round either.
	z := &missWindow{}
	z.noteSetStart(setStart{epoch: 0, reads: 0, set: 1})
	z.noteSetStart(setStart{epoch: 1, reads: 2, set: 2})
	if z.keptAsEarlierSet(miss(models.RoundUnmarked, 0, 0, "")) {
		t.Error("an unmarked miss is kept as an earlier set's call where the window's first start is at epoch 0")
	}
	if !z.keptAsEarlierSet(miss(settled, 0, 1, "")) {
		t.Error("precondition: a marked miss of that first set before its last read is not kept")
	}
	// A mark whose start is the running set's, or after it, names 1.
	if got := w.setsBack(miss(settled, 6, 5, ""), 3); got != 1 {
		t.Errorf("a mark of the running set names %d set(s) back; want 1", got)
	}
	if got := w.setsBack(miss(settled, 6, 5, ""), 2); got != 1 {
		t.Errorf("a mark of a set after the running one names %d set(s) back; want 1", got)
	}
}
