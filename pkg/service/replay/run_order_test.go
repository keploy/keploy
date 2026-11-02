package replay

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// orderOf is the test of each row, one per line, as the report lists them.
func orderOf(rows []models.TestResult) []byte {
	var b bytes.Buffer
	for _, r := range rows {
		fmt.Fprintln(&b, r.TestCaseID)
	}
	return b.Bytes()
}

func ptrOrderOf(rows []*models.TestResult) []byte {
	var b bytes.Buffer
	for _, r := range rows {
		fmt.Fprintln(&b, r.TestCaseID)
	}
	return b.Bytes()
}

// testNames is test-1 … test-n, the harness's run order.
func testNames(n int) []byte {
	var b bytes.Buffer
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "test-%d\n", i)
	}
	return b.Bytes()
}

// A RetryPassing cycle that judges a test again replaces its row where the
// first cycle put it, and the rows come back in the order the loop first
// judged them. Many tests, read many times: a map's order would show.
func TestJudgedRowsKeepTheOrderTheTestsWereFirstJudgedIn(t *testing.T) {
	const n = 64
	j := newJudgedRows()
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("test-%d", i)
		j.put(name, &models.TestResult{TestCaseID: name, Status: models.TestStatusPassed})
	}
	for i := 2; i <= n; i += 3 {
		name := fmt.Sprintf("test-%d", i)
		j.put(name, &models.TestResult{TestCaseID: name, Status: models.TestStatusFailed})
	}
	for read := 0; read < 20; read++ {
		rows := j.inRunOrder()
		if got, want := ptrOrderOf(rows), testNames(n); !bytes.Equal(got, want) {
			t.Fatalf("read %d: rows in order\n%s\nwant\n%s", read, got, want)
		}
		for i, r := range rows {
			want := models.TestStatusPassed
			if (i+1)%3 == 2 {
				want = models.TestStatusFailed
			}
			if r.Status != want {
				t.Fatalf("%s is %s, want its last cycle's row (%s)", r.TestCaseID, r.Status, want)
			}
		}
	}
}

// The report lists the set's tests in the order the set ran them, also when
// a test's row was written at once (no answer) and the others only when the
// loop ended, and the store holds the judged rows in that order too.
func TestRunTestSetWritesTheReportsTestsInRunOrder(t *testing.T) {
	const n = 40
	h := newPartialRunHarness(t, n, 0)
	h.replayer.hookImpl = prHooks{simErr: map[string]error{"test-17": errors.New("connection reset by peer")}}
	_ = h.run(t)
	if h.report.report == nil {
		t.Fatal("precondition: the set wrote no report")
	}
	if got, want := orderOf(h.report.report.Tests), testNames(n); !bytes.Equal(got, want) {
		t.Fatalf("the report lists its tests in order\n%s\nwant the run order\n%s", got, want)
	}
	if r := h.report.report.Tests[16]; r.TestCaseID != "test-17" || r.Status != models.TestStatusFailed {
		t.Fatalf("precondition: test-17 is %s %s, want a FAILED row (no answer)", r.TestCaseID, r.Status)
	}
	h.report.mu.Lock()
	stored := append([]models.TestResult(nil), h.report.results...)
	h.report.mu.Unlock()
	var judged []models.TestResult
	for _, r := range stored {
		if r.TestCaseID != "test-17" {
			judged = append(judged, r)
		}
	}
	want := strings.Replace(string(testNames(n)), "test-17\n", "", 1)
	if got := orderOf(judged); string(got) != want {
		t.Fatalf("the store holds the judged rows in order\n%s\nwant the run order\n%s", got, want)
	}
}

// Two runs of one set write its tests in byte-identical order.
func TestRunTestSetTwoRunsWriteOneTestOrder(t *testing.T) {
	const n = 40
	var orders [2][]byte
	for run := range orders {
		h := newPartialRunHarness(t, n, 0)
		_ = h.run(t)
		if h.report.report == nil {
			t.Fatalf("run %d: precondition: the set wrote no report", run+1)
		}
		orders[run] = orderOf(h.report.report.Tests)
	}
	if !bytes.Equal(orders[0], orders[1]) {
		t.Fatalf("two runs of one set listed its tests\n%s\nand\n%s", orders[0], orders[1])
	}
	if !bytes.Equal(orders[0], testNames(n)) {
		t.Fatalf("the runs listed the tests\n%s\nwant the run order\n%s", orders[0], testNames(n))
	}
}

// A row of a test the set did not place keeps its place after every placed
// one, and a test's two rows keep their order.
func TestRunOrderSortIsStable(t *testing.T) {
	o := newRunOrder()
	for _, n := range []string{"b", "a", "c"} {
		o.note(n)
	}
	o.note("b") // a test keeps its first place
	rows := []models.TestResult{
		{TestCaseID: "c"}, {TestCaseID: "x"}, {TestCaseID: "a", Status: models.TestStatusFailed},
		{TestCaseID: "b"}, {TestCaseID: "a", Status: models.TestStatusPassed}, {TestCaseID: "y"},
	}
	o.sort(rows)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.TestCaseID+string(r.Status))
	}
	want := "b a" + string(models.TestStatusFailed) + " a" + string(models.TestStatusPassed) + " c x y"
	if strings.Join(got, " ") != want {
		t.Fatalf("sorted %q, want %q", strings.Join(got, " "), want)
	}
}
