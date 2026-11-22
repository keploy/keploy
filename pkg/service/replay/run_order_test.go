package replay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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

// The sort is stable over more rows than an insertion sort handles: Go's
// sort.Slice happens to keep the order of 12 rows or fewer.
func TestRunOrderSortIsStableOverManyRows(t *testing.T) {
	o := newRunOrder()
	for i := 0; i < 5; i++ {
		o.note(fmt.Sprintf("t%d", i))
	}
	var rows []models.TestResult
	for k := 0; k < 40; k++ {
		for i := 4; i >= 0; i-- {
			rows = append(rows, models.TestResult{TestCaseID: fmt.Sprintf("t%d", i), Name: fmt.Sprintf("%03d", k)})
		}
	}
	o.sort(rows)
	for i := 1; i < len(rows); i++ {
		if rows[i].TestCaseID < rows[i-1].TestCaseID {
			t.Fatalf("not in run order at %d: %s after %s", i, rows[i].TestCaseID, rows[i-1].TestCaseID)
		}
		if rows[i].TestCaseID == rows[i-1].TestCaseID && rows[i].Name < rows[i-1].Name {
			t.Fatalf("the rows of %s left their order at %d: %s before %s", rows[i].TestCaseID, i, rows[i-1].Name, rows[i].Name)
		}
	}
}

// take gives a test's row back once, and the test leaves the order.
func TestJudgedRowsTakeLeavesTheOrder(t *testing.T) {
	j := newJudgedRows()
	for _, n := range []string{"a", "b", "c"} {
		j.put(n, &models.TestResult{TestCaseID: n})
	}
	if r := j.take("b"); r == nil || r.TestCaseID != "b" {
		t.Fatalf("take(b) = %v, want b's row", r)
	}
	if r := j.take("b"); r != nil {
		t.Fatalf("take(b) again = %v, want nil", r)
	}
	if got, want := ptrOrderOf(j.inRunOrder()), []byte("a\nc\n"); !bytes.Equal(got, want) {
		t.Fatalf("rows after take\n%s\nwant\n%s", got, want)
	}
}

// An IGNORED test, judged before the walk, comes first, and a streaming test,
// deferred past the walk, comes last.
func TestRunTestSetIgnoredFirstStreamingLast(t *testing.T) {
	h := newPartialRunHarness(t, 8, 0)
	h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-6"}}
	h.streaming("test-3")
	_ = h.run(t)
	if h.report.report == nil {
		t.Fatal("precondition: the set wrote no report")
	}
	got := orderOf(h.report.report.Tests)
	want := []byte("test-6\ntest-1\ntest-2\ntest-4\ntest-5\ntest-7\ntest-8\ntest-3\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("the report lists its tests in order\n%s\nwant\n%s", got, want)
	}
}

// retryRun is what a RetryPassing run's hooks and agent share: how many times
// each test ran, and which test ran last.
type retryRun struct {
	mu    sync.Mutex
	calls map[string]int
	cur   string
}

// cycleHooks: the wrongAll tests always answer wrong (they fail cycle 1, so
// the passing tests run again); the noAnswerFrom tests get no answer from
// their n-th run on.
type cycleHooks struct {
	prHooks
	run          *retryRun
	wrongAll     map[string]bool
	noAnswerFrom map[string]int
}

func (h cycleHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	h.run.mu.Lock()
	h.run.calls[tc.Name]++
	h.run.cur = tc.Name
	n := h.run.calls[tc.Name]
	h.run.mu.Unlock()
	if from, ok := h.noAnswerFrom[tc.Name]; ok && n >= from {
		return nil, errors.New("connection reset by peer")
	}
	resp, err := h.prHooks.SimulateRequest(ctx, tc, set)
	if r, ok := resp.(*models.HTTPResp); ok && h.wrongAll[tc.Name] {
		r.Body = `{"ok":false}`
	}
	return resp, err
}

// missOnRun is the agent of a retryRun: on test's nth run, the test made a
// call no mock answered (miss).
type missOnRun struct {
	*prInstr
	run  *retryRun
	test string
	nth  int
	miss models.UnmatchedCall
}

func (m *missOnRun) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	m.run.mu.Lock()
	defer m.run.mu.Unlock()
	if m.run.cur == m.test && m.run.calls[m.test] == m.nth {
		return []models.UnmatchedCall{m.miss}, nil
	}
	return nil, nil
}

// With RetryPassing, test-3 passes cycle 1 and gets no answer in cycle 2. Its
// rows are written cycle by cycle: cycle 1's PASSED, then cycle 2's FAILED, so
// what reads a test's last row (lastResultOfEachTest, returnedMissed) reads
// cycle 2's. Each cycle's row stays, and the set counts test-3 once.
func TestRunTestSetRetryPassingWritesATestsRowsCycleByCycle(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", on), func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			h := newPartialRunHarness(t, 4, 0)
			h.replayer.config.RetryPassing = true
			run := &retryRun{calls: map[string]int{}}
			h.replayer.hookImpl = cycleHooks{run: run, wrongAll: map[string]bool{"test-2": true}, noAnswerFrom: map[string]int{"test-3": 2}}
			miss := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /gone", ClosestMock: "mock-given-back"}
			h.replayer.instrumentation = &missOnRun{prInstr: h.instr, run: run, test: "test-3", nth: 2, miss: miss}
			_ = h.run(t)
			rep := h.report.report
			if rep == nil {
				t.Fatal("precondition: the set wrote no report")
			}
			run.mu.Lock()
			ran := run.calls["test-3"]
			run.mu.Unlock()
			if ran != 2 {
				t.Fatalf("precondition: test-3 ran %d time(s), want 2 (a second cycle)", ran)
			}
			var got []string
			for _, r := range rep.Tests {
				got = append(got, r.TestCaseID+":"+string(r.Status))
			}
			want := []string{"test-1:PASSED", "test-2:FAILED", "test-3:PASSED", "test-3:FAILED", "test-4:PASSED"}
			if !slices.Equal(got, want) {
				t.Fatalf("the report's rows are\n%v\nwant\n%v", got, want)
			}
			for _, r := range lastResultOfEachTest(rep.Tests) {
				if r.TestCaseID == "test-3" && r.Status != models.TestStatusFailed {
					t.Fatalf("test-3's last row is %s, want its last cycle's (FAILED)", r.Status)
				}
			}
			plan := &exactPlan{returned: map[string]string{"mock-given-back": "test-1"}}
			back := plan.returnedMissed(rep.Tests)
			if wantBack := []string{"test-3: GET /{} (mock-given-back, given back to test-1)"}; !slices.Equal(back, wantBack) {
				t.Fatalf("returnedMissed = %q, want cycle 2's miss %q", back, wantBack)
			}
			if rep.Success != 2 || rep.Failure != 2 || rep.Total != 4 {
				t.Fatalf("the set counts passed %d, failed %d of %d; want 2, 2 of 4 (test-3 once)", rep.Success, rep.Failure, rep.Total)
			}
		})
	}
}

// secondRunHooks: test-2 fails every run (a wrong body over HTTP, no answer
// over gRPC), so the passing tests run again; test, on its second run, gets
// what second answers.
type secondRunHooks struct {
	prHooks
	run    *retryRun
	test   string
	second func(tc *models.TestCase) (interface{}, error)
}

func (h secondRunHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	h.run.mu.Lock()
	h.run.calls[tc.Name]++
	h.run.cur = tc.Name
	n := h.run.calls[tc.Name]
	h.run.mu.Unlock()
	if tc.Name == h.test && n == 2 {
		return h.second(tc)
	}
	if tc.Name == "test-2" && tc.Kind == models.GRPC_EXPORT {
		return nil, errors.New("connection reset by peer")
	}
	resp, err := h.prHooks.SimulateRequest(ctx, tc, set)
	if r, ok := resp.(*models.HTTPResp); ok && tc.Name == "test-2" {
		r.Body = `{"ok":false}`
	}
	return resp, err
}

// With RetryPassing, test-3 passes cycle 1 and fails cycle 2 on each path
// that writes its row at once: no answer, an answer judged on its start (a
// status line, then a drop), and an answer of the wrong type over HTTP and
// over gRPC. On each, cycle 1's PASSED row is written before cycle 2's
// FAILED row, so test-3's last row is its last cycle's.
func TestRunTestSetEveryAtOnceWriterOfALaterCycleWritesTheEarlierRowFirst(t *testing.T) {
	paths := []struct {
		name   string
		grpc   bool
		second func(tc *models.TestCase) (interface{}, error)
	}{
		{"no answer", false, func(*models.TestCase) (interface{}, error) {
			return nil, errors.New("connection reset by peer")
		}},
		{"start judged", false, func(tc *models.TestCase) (interface{}, error) {
			return &models.HTTPResp{StatusCode: 503, Header: tc.HTTPResp.Header}, &pkg.AnswerStallError{Target: "app", Answered: true}
		}},
		{"wrong answer type", false, func(*models.TestCase) (interface{}, error) {
			return &models.GrpcResp{}, nil
		}},
		{"grpc wrong answer type", true, func(*models.TestCase) (interface{}, error) {
			return &models.HTTPResp{}, nil
		}},
	}
	for _, p := range paths {
		for _, on := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/proven=%v", p.name, on), func(t *testing.T) {
				if on {
					t.Setenv(provenswitch.Env, "1")
				} else {
					t.Setenv(provenswitch.Env, "")
				}
				h := newPartialRunHarness(t, 4, 0)
				if p.grpc {
					for _, tc := range h.cases {
						tc.Kind = models.GRPC_EXPORT
					}
				}
				h.replayer.config.RetryPassing = true
				run := &retryRun{calls: map[string]int{}}
				h.replayer.hookImpl = secondRunHooks{run: run, test: "test-3", second: p.second}
				_ = h.run(t)
				rep := h.report.report
				if rep == nil {
					t.Fatal("precondition: the set wrote no report")
				}
				run.mu.Lock()
				ran := run.calls["test-3"]
				run.mu.Unlock()
				if ran != 2 {
					t.Fatalf("precondition: test-3 ran %d time(s), want 2 (a second cycle)", ran)
				}
				var got []string
				for _, r := range rep.Tests {
					got = append(got, r.TestCaseID+":"+string(r.Status))
				}
				want := []string{"test-1:PASSED", "test-2:FAILED", "test-3:PASSED", "test-3:FAILED", "test-4:PASSED"}
				if !slices.Equal(got, want) {
					t.Fatalf("the report's rows are\n%v\nwant\n%v", got, want)
				}
				for _, r := range lastResultOfEachTest(rep.Tests) {
					if r.TestCaseID == "test-3" && r.Status != models.TestStatusFailed {
						t.Fatalf("test-3's last row is %s, want its last cycle's (FAILED)", r.Status)
					}
				}
			})
		}
	}
}

// renameHooks renames a test's row in BeforeTestResult.
type renameHooks struct {
	prHooks
	from, to string
}

func (h renameHooks) BeforeTestResult(_ context.Context, _, _ string, results []models.TestResult) error {
	for i := range results {
		if results[i].TestCaseID == h.from {
			results[i].TestCaseID = h.to
		}
	}
	return nil
}

// Under the proven switch the evidence floor puts back a test a hook renamed:
// the test takes its place in the run, and the renamed row comes after every
// test.
func TestRunTestSetFloorPutsATestBackInItsPlace(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 5, 0)
	h.replayer.hookImpl = renameHooks{from: "test-3", to: "test-3-renamed"}
	_ = h.run(t)
	if h.report.report == nil {
		t.Fatal("precondition: the set wrote no report")
	}
	got := orderOf(h.report.report.Tests)
	want := []byte("test-1\ntest-2\ntest-3\ntest-4\ntest-5\ntest-3-renamed\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("the report lists its tests in order\n%s\nwant\n%s", got, want)
	}
	if r := h.report.report.Tests[2]; r.Status != models.TestStatusFailed {
		t.Fatalf("precondition: the floor put test-3 back %s, want FAILED", r.Status)
	}
}
