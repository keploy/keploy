package replay

import (
	"slices"
	"sort"

	"go.keploy.io/server/v3/pkg/models"
)

// runOrder is each test's place in the order a test set runs and judges its
// tests: the order keploy test walks them, by request and then response
// timestamp (testdb.GetTestCases), the tests it judges before the walk
// (IGNORED) first and the streaming tests it defers last. A report lists its
// tests in it, so two runs of one recording list them alike and a reader can
// take a test's place in the report for its place in the run.
type runOrder struct {
	place map[string]int
}

func newRunOrder() *runOrder {
	return &runOrder{place: make(map[string]int)}
}

// note gives the test the next place, unless it has one.
func (o *runOrder) note(name string) {
	if _, ok := o.place[name]; !ok {
		o.place[name] = len(o.place)
	}
}

// at is the test's place; a test the set did not place comes after every
// test it did.
func (o *runOrder) at(name string) int {
	if p, ok := o.place[name]; ok {
		return p
	}
	return len(o.place)
}

// sort puts rows in run order. The sort is stable: a test's rows keep the
// order they were written in, which is the order of the cycles that wrote them
// (judgedRows.take), and so do the rows of tests the set did not place.
func (o *runOrder) sort(rows []models.TestResult) {
	sort.SliceStable(rows, func(i, j int) bool {
		return o.at(rows[i].TestCaseID) < o.at(rows[j].TestCaseID)
	})
}

// judgedRows is the row the test loop judged for each test, beside the order
// it first judged them in. A later RetryPassing cycle that judges a test
// again replaces its row and keeps its place; one that writes the test's row
// at once (no answer) takes the earlier cycle's row out first (take).
type judgedRows struct {
	row   map[string]*models.TestResult
	order []string
}

func newJudgedRows() *judgedRows {
	return &judgedRows{row: make(map[string]*models.TestResult)}
}

func (j *judgedRows) put(name string, res *models.TestResult) {
	if _, ok := j.row[name]; !ok {
		j.order = append(j.order, name)
	}
	j.row[name] = res
}

// inRunOrder is the rows in the order the loop first judged their tests.
func (j *judgedRows) inRunOrder() []*models.TestResult {
	out := make([]*models.TestResult, 0, len(j.order))
	for _, name := range j.order {
		out = append(out, j.row[name])
	}
	return out
}

// take is the test's row, taken out, or nil when no cycle judged the test
// yet. A later cycle that writes the test's row to the store at once writes
// this one before it, so the store holds the test's rows cycle by cycle and
// its last row is its last cycle's (lastResultOfEachTest).
func (j *judgedRows) take(name string) *models.TestResult {
	res, ok := j.row[name]
	if !ok {
		return nil
	}
	delete(j.row, name)
	j.order = slices.DeleteFunc(j.order, func(n string) bool { return n == name })
	return res
}
