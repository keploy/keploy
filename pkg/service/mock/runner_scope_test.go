package mock

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestParseRunnerLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want scopeEvent
	}{
		{"json run is a begin, qualified by package, at the runner's time", `{"Time":"2026-09-24T10:00:00.000123456Z","Action":"run","Package":"github.com/acme/shop/cart","Test":"TestAdd"}`,
			scopeEvent{kind: scopeBegin, pkg: "github.com/acme/shop/cart", test: "TestAdd", at: time.Date(2026, 9, 24, 10, 0, 0, 123456, time.UTC)}},
		{"json time with an offset", `{"Time":"2026-09-24T15:30:00.5+05:30","Action":"pass","Package":"cart","Test":"TestAdd"}`,
			scopeEvent{kind: scopeEnd, pkg: "cart", test: "TestAdd", at: time.Date(2026, 9, 24, 10, 0, 0, 500000000, time.UTC), status: "pass"}},
		{"json with an unreadable time still marks the boundary", `{"Time":"yesterday","Action":"run","Package":"cart","Test":"TestAdd"}`,
			scopeEvent{kind: scopeBegin, pkg: "cart", test: "TestAdd"}},
		{"json pass is an end, with how long it took", `{"Action":"pass","Package":"cart","Test":"TestAdd","Elapsed":0.01}`, scopeEvent{kind: scopeEnd, pkg: "cart", test: "TestAdd", status: "pass", elapsed: 10 * time.Millisecond}},
		{"json fail is an end", `{"Action":"fail","Package":"cart","Test":"TestAdd"}`, scopeEvent{kind: scopeEnd, pkg: "cart", test: "TestAdd", status: "fail"}},
		{"json skip is an end", `{"Action":"skip","Package":"cart","Test":"TestAdd/empty"}`, scopeEvent{kind: scopeEnd, pkg: "cart", test: "TestAdd/empty", status: "skip"}},
		{"json subtest keeps its full path", `{"Action":"run","Package":"cart","Test":"TestAdd/two_items"}`, scopeEvent{kind: scopeBegin, pkg: "cart", test: "TestAdd/two_items"}},
		{"json without a package uses the bare test", `{"Action":"run","Test":"TestAdd"}`, scopeEvent{kind: scopeBegin, test: "TestAdd"}},
		{"json pause is a pause", `{"Action":"pause","Package":"cart","Test":"TestAdd"}`, scopeEvent{kind: scopePause, pkg: "cart", test: "TestAdd"}},
		{"json output is ignored", `{"Action":"output","Package":"cart","Test":"TestAdd","Output":"=== RUN   TestAdd\n"}`, scopeEvent{}},
		{"json cont is ignored", `{"Action":"cont","Package":"cart","Test":"TestAdd"}`, scopeEvent{}},
		{"json package event without a test is ignored", `{"Action":"pass","Package":"cart","Elapsed":0.2}`, scopeEvent{}},
		{"broken json is ignored", `{"Action":"run","Test":`, scopeEvent{}},
		{"plain run", "=== RUN   TestAdd", scopeEvent{kind: scopeBegin, test: "TestAdd"}},
		{"plain indented subtest run", "    === RUN   TestAdd/two_items", scopeEvent{kind: scopeBegin, test: "TestAdd/two_items"}},
		{"plain pass with duration", "--- PASS: TestAdd (0.01s)", scopeEvent{kind: scopeEnd, test: "TestAdd", status: "pass", elapsed: 10 * time.Millisecond}},
		{"plain indented subtest pass", "    --- PASS: TestAdd/two_items (0.00s)", scopeEvent{kind: scopeEnd, test: "TestAdd/two_items", status: "pass"}},
		{"plain fail", "--- FAIL: TestAdd (0.01s)", scopeEvent{kind: scopeEnd, test: "TestAdd", status: "fail", elapsed: 10 * time.Millisecond}},
		{"plain skip", "        --- SKIP: TestAdd/empty (0.00s)", scopeEvent{kind: scopeEnd, test: "TestAdd/empty", status: "skip"}},
		{"plain result without a duration", "--- FAIL: TestAdd", scopeEvent{kind: scopeEnd, test: "TestAdd", status: "fail"}},
		{"plain pause", "=== PAUSE TestAdd", scopeEvent{kind: scopePause, test: "TestAdd"}},
		{"plain cont is ignored", "=== CONT  TestAdd", scopeEvent{}},
		{"plain name marker is ignored", "=== NAME  TestAdd", scopeEvent{}},
		{"windows line ending", "=== RUN   TestAdd\r", scopeEvent{kind: scopeBegin, test: "TestAdd"}},
		{"test log line is ignored", "    cart_test.go:12: got 2 items", scopeEvent{}},
		{"summary lines are ignored", "ok  \tgithub.com/acme/shop/cart\t0.012s", scopeEvent{}},
		{"PASS alone is ignored", "PASS", scopeEvent{}},
		{"empty line is ignored", "", scopeEvent{}},
		{"compose service prefix is dropped", "tests-1  | === RUN   TestAdd", scopeEvent{kind: scopeBegin, test: "TestAdd"}},
		{"compose prefix on json", `tests-1  | {"Action":"run","Package":"cart","Test":"TestAdd"}`, scopeEvent{kind: scopeBegin, pkg: "cart", test: "TestAdd"}},
		{"colour codes are dropped", "\x1b[36mtests-1  | \x1b[0m--- PASS: TestAdd (0.00s)", scopeEvent{kind: scopeEnd, test: "TestAdd", status: "pass"}},
		{"a pipe inside a log line is not a prefix", "    cart_test.go:12: got a | b", scopeEvent{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRunnerLine(tc.line)
			require.True(t, got.at.Equal(tc.want.at), "time: got %v want %v", got.at, tc.want.at)
			got.at, tc.want.at = time.Time{}, time.Time{}
			require.Equal(t, tc.want, got)
		})
	}
}

func TestScopeEventName(t *testing.T) {
	require.Equal(t, "cart.TestAdd/x", scopeEvent{pkg: "cart", test: "TestAdd/x"}.name())
	require.Equal(t, "TestAdd", scopeEvent{test: "TestAdd"}.name())
}

// fakeMarker records the scope calls in order, and the time each one carried.
type fakeMarker struct {
	mu    sync.Mutex
	calls []string
	ats   []time.Time
}

func (f *fakeMarker) BeginScope(_ context.Context, name string, pid int, at time.Time) error {
	return f.add("begin", name, pid, at)
}

func (f *fakeMarker) EndScope(_ context.Context, name string, pid int, at time.Time) error {
	return f.add("end", name, pid, at)
}

func (f *fakeMarker) add(mark, name string, pid int, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pid != 0 {
		f.calls = append(f.calls, "unexpected pid")
	}
	f.calls = append(f.calls, mark+" "+name)
	f.ats = append(f.ats, at)
	return nil
}

func feed(t *testing.T, output string) (*fakeMarker, *runnerScope) {
	t.Helper()
	marker := &fakeMarker{}
	scope := newRunnerScope(context.Background(), zap.NewNop(), marker)
	// Written in odd-sized pieces so a line split across writes is exercised.
	for len(output) > 0 {
		n := min(7, len(output))
		wrote, err := scope.Write([]byte(output[:n]))
		require.NoError(t, err)
		require.Equal(t, n, wrote)
		output = output[n:]
	}
	return marker, scope
}

// What a compiled test binary prints with -test.v: subtest results come only after the parent's.
const plainSequential = `=== RUN   TestA
=== RUN   TestA/one
=== RUN   TestA/two
    probe_test.go:7: skipping
--- PASS: TestA (0.00s)
    --- PASS: TestA/one (0.00s)
    --- SKIP: TestA/two (0.00s)
=== RUN   TestC
--- PASS: TestC (0.00s)
PASS
`

func TestRunnerScopePostsEachBoundaryOnce(t *testing.T) {
	marker, scope := feed(t, plainSequential)
	require.Equal(t, []string{
		"begin TestA", "begin TestA/one", "begin TestA/two",
		"end TestA", "end TestA/one", "end TestA/two",
		"begin TestC", "end TestC",
	}, marker.calls)
	require.Empty(t, scope.overlapping())
}

func TestRunnerScopeSeesNoOverlapInSequentialSubtests(t *testing.T) {
	_, scope := feed(t, `{"Action":"run","Package":"p","Test":"TestA"}
{"Action":"run","Package":"p","Test":"TestA/one"}
{"Action":"pass","Package":"p","Test":"TestA/one"}
{"Action":"run","Package":"p","Test":"TestA/two"}
{"Action":"output","Package":"p","Test":"TestA/two","Output":"=== RUN   TestZ\n"}
{"Action":"skip","Package":"p","Test":"TestA/two"}
{"Action":"pass","Package":"p","Test":"TestA"}
{"Action":"run","Package":"p","Test":"TestB"}
{"Action":"pass","Package":"p","Test":"TestB"}
`)
	require.Empty(t, scope.overlapping())
}

func TestRunnerScopeFlagsParallelSubtests(t *testing.T) {
	_, scope := feed(t, `=== RUN   TestB
=== RUN   TestB/p1
=== PAUSE TestB/p1
=== RUN   TestB/p2
=== PAUSE TestB/p2
=== CONT  TestB/p1
=== CONT  TestB/p2
--- PASS: TestB (0.00s)
    --- PASS: TestB/p1 (0.00s)
    --- PASS: TestB/p2 (0.00s)
`)
	require.Equal(t, []string{"TestB/p1 and TestB/p2"}, scope.overlapping())
}

func TestRunnerScopeFlagsParallelTopLevelTests(t *testing.T) {
	_, scope := feed(t, `{"Action":"run","Package":"p","Test":"TestA"}
{"Action":"pause","Package":"p","Test":"TestA"}
{"Action":"run","Package":"p","Test":"TestB"}
{"Action":"pause","Package":"p","Test":"TestB"}
{"Action":"cont","Package":"p","Test":"TestA"}
{"Action":"cont","Package":"p","Test":"TestB"}
{"Action":"pass","Package":"p","Test":"TestA"}
{"Action":"pass","Package":"p","Test":"TestB"}
`)
	require.Equal(t, []string{"p.TestA and p.TestB"}, scope.overlapping())
}

func TestRunnerScopeFlagsPackagesRunningTogether(t *testing.T) {
	_, scope := feed(t, `{"Action":"run","Package":"github.com/acme/a","Test":"TestX"}
{"Action":"run","Package":"github.com/acme/b","Test":"TestY"}
{"Action":"pass","Package":"github.com/acme/a","Test":"TestX"}
{"Action":"pass","Package":"github.com/acme/b","Test":"TestY"}
`)
	require.Equal(t, []string{"github.com/acme/a.TestX and github.com/acme/b.TestY"}, scope.overlapping())
}

// go test -json carries the runner's own clock; plain output has none, so those boundaries get stamped on read.
func TestRunnerScopePassesTheRunnerTime(t *testing.T) {
	marker, scope := feed(t, `{"Time":"2026-09-24T10:00:00.000123456Z","Action":"run","Package":"orders/e2e","Test":"TestA"}
{"Time":"2026-09-24T10:00:00.5Z","Action":"pass","Package":"orders/e2e","Test":"TestA"}
=== RUN   TestB
--- PASS: TestB (0.00s)
`)
	require.Equal(t, []string{"begin orders/e2e.TestA", "end orders/e2e.TestA", "begin TestB", "end TestB"}, marker.calls)
	require.True(t, marker.ats[0].Equal(time.Date(2026, 9, 24, 10, 0, 0, 123456, time.UTC)))
	require.True(t, marker.ats[1].Equal(time.Date(2026, 9, 24, 10, 0, 0, 500000000, time.UTC)))
	require.True(t, marker.ats[2].IsZero())
	require.True(t, marker.ats[3].IsZero())
	require.True(t, scope.usedReadTime())
}

func TestRunnerScopeWithTimedEventsNeverUsesReadTime(t *testing.T) {
	_, scope := feed(t, `{"Time":"2026-09-24T10:00:00Z","Action":"run","Package":"p","Test":"TestA"}
{"Time":"2026-09-24T10:00:01Z","Action":"pass","Package":"p","Test":"TestA"}
`)
	require.False(t, scope.usedReadTime())
}

func TestRunnerScopeIgnoresUnrelatedOutput(t *testing.T) {
	marker, scope := feed(t, "collected 3 items\n\ntest_cart.py::test_add PASSED\n=== something else\n")
	require.Empty(t, marker.calls)
	require.Empty(t, scope.overlapping())
}

var runnerT0 = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

func sameWindows(t *testing.T, want, got []models.ScopeWindow) {
	t.Helper()
	require.Len(t, got, len(want), "windows %+v", got)
	for i := range want {
		require.Equal(t, want[i].Name, got[i].Name)
		require.True(t, got[i].Start.Equal(want[i].Start), "%s start %v want %v", want[i].Name, got[i].Start, want[i].Start)
		require.True(t, got[i].End.Equal(want[i].End), "%s end %v want %v", want[i].Name, got[i].End, want[i].End)
		require.Equal(t, want[i].PID, got[i].PID)
	}
}

// go test -json events carry the runner's clock, so the windows are the runner's, not the moment keploy read them.
func TestRunnerScopeBuildsWindowsFromTheRunnerClock(t *testing.T) {
	_, scope := feed(t, `{"Time":"2026-09-24T10:00:00Z","Action":"run","Package":"orders/e2e","Test":"TestA"}
{"Time":"2026-09-24T10:00:00.002Z","Action":"run","Package":"orders/e2e","Test":"TestA/one"}
{"Time":"2026-09-24T10:00:00.004Z","Action":"pass","Package":"orders/e2e","Test":"TestA/one"}
{"Time":"2026-09-24T10:00:00.01Z","Action":"pass","Package":"orders/e2e","Test":"TestA"}
{"Time":"2026-09-24T10:00:00.01Z","Action":"run","Package":"orders/e2e","Test":"TestC"}
{"Time":"2026-09-24T10:00:00.02Z","Action":"fail","Package":"orders/e2e","Test":"TestC"}
`)
	sameWindows(t, []models.ScopeWindow{
		{Name: "orders/e2e.TestA/one", Start: runnerT0.Add(2 * time.Millisecond), End: runnerT0.Add(4 * time.Millisecond)},
		{Name: "orders/e2e.TestA", Start: runnerT0, End: runnerT0.Add(10 * time.Millisecond)},
		{Name: "orders/e2e.TestC", Start: runnerT0.Add(10 * time.Millisecond), End: runnerT0.Add(20 * time.Millisecond)},
	}, scope.windows())
	require.False(t, scope.usedReadTime())
}

// Plain output has no clock, so windows are stamped on read; a subtest's result prints only after its parent's, so its window closes when the next sibling starts.
func TestRunnerScopeStampsPlainWindowsOnRead(t *testing.T) {
	before := time.Now()
	_, scope := feed(t, plainSequential)
	after := time.Now()

	ws := scope.windows()
	names := make([]string, 0, len(ws))
	for _, w := range ws {
		names = append(names, w.Name)
		require.False(t, w.Start.Before(before), "%s started before the output was read", w.Name)
		require.False(t, w.End.After(after), "%s ended after the output was read", w.Name)
		require.False(t, w.End.Before(w.Start), "%s ends before it starts", w.Name)
	}
	require.Equal(t, []string{"TestA/one", "TestA", "TestA/two", "TestC"}, names)
	require.True(t, ws[0].End.Equal(ws[2].Start), "TestA/one must close exactly where TestA/two opens")
	require.True(t, scope.usedReadTime())
}

func TestRunnerScopeLeavesAnUnfinishedTestOut(t *testing.T) {
	_, scope := feed(t, "=== RUN   TestA\n=== RUN   TestA/one\n")
	require.Empty(t, scope.windows())
}

// The adapter's window carries the runner's clock; the agent's copy of the same test was stamped on receipt and loses.
func TestMergeWindowsPrefersTheTestsOwnMarks(t *testing.T) {
	adapter := []models.ScopeWindow{{Name: "p.TestA", Start: ts(10), End: ts(20)}, {Name: "p.TestB", Start: ts(20), End: ts(30)}}
	agent := []models.ScopeWindow{
		{Name: "p.TestA", Start: ts(11), End: ts(21), PID: 7},
		{Name: "fixture.Setup", Start: ts(30), End: ts(40), PID: 7},
	}
	sameWindows(t, []models.ScopeWindow{
		{Name: "p.TestA", Start: ts(11), End: ts(21), PID: 7},
		{Name: "fixture.Setup", Start: ts(30), End: ts(40), PID: 7},
		{Name: "p.TestB", Start: ts(20), End: ts(30)},
	}, mergeWindows(adapter, agent))
	sameWindows(t, agent, mergeWindows(nil, agent))
	sameWindows(t, adapter, mergeWindows(adapter, nil))
}

func TestNearBoundaryCountsMocksAtAWindowEdge(t *testing.T) {
	windows := []models.ScopeWindow{
		{Name: "t1", Start: ts(10), End: ts(20)},
		{Name: "t2", Start: ts(20), End: ts(30)},
	}
	mocks := []capturedMock{
		{name: "just after a start", ts: ts(10).Add(5 * time.Millisecond)},
		{name: "just before an end", ts: ts(20).Add(-15 * time.Millisecond)},
		{name: "on the shared edge, counted once", ts: ts(20)},
		{name: "just before a start", ts: ts(10).Add(-20 * time.Millisecond)},
		{name: "safely inside", ts: ts(15)},
		{name: "too far after", ts: ts(30).Add(21 * time.Millisecond)},
	}
	require.Equal(t, 4, nearBoundary(windows, mocks, boundarySlack))
	require.Equal(t, 0, nearBoundary(nil, mocks, boundarySlack))
}

func TestFirstFew(t *testing.T) {
	require.Equal(t, "a, b", firstFew([]string{"a", "b"}, 10))
	require.Equal(t, "a, b and 3 more", firstFew(strings.Split("a b c d e", " "), 2))
}

func TestNilRunnerScopeIsOff(t *testing.T) {
	var scope *runnerScope
	require.Nil(t, scope.writer())
	require.Nil(t, scope.overlapping())
	require.Nil(t, scope.windows())
	require.Nil(t, scope.tests())
	require.False(t, scope.usedReadTime())
}

// The runner prints a test's start a moment after it began: a call in that gap belongs to the next test.
func TestSealedWindowsLeaveNoGapBetweenSequentialTests(t *testing.T) {
	at := func(ms int) time.Time { return runnerT0.Add(time.Duration(ms) * time.Microsecond) }
	ws := sealed([]models.ScopeWindow{
		{Name: "TestGetOrder", Start: at(706293), End: at(708854)},
		{Name: "TestCreateOrder", Start: at(635078), End: at(705971)},
	})
	require.Equal(t, "TestGetOrder", ws[0].Name, "the order the runner closed them in is kept")
	require.Equal(t, at(705971), ws[0].Start, "the second test starts where the first ended")
	require.Equal(t, at(708854), ws[0].End)
	require.Equal(t, at(635078), ws[1].Start, "the first test keeps its own start")
	call := at(706124)
	require.True(t, !call.Before(ws[0].Start) && !call.After(ws[0].End), "the pricing call made in the gap now lands in TestGetOrder")

	parallel := sealed([]models.ScopeWindow{
		{Name: "TestA", Start: at(0), End: at(500)},
		{Name: "TestB", Start: at(100), End: at(600)},
	})
	require.Equal(t, at(100), parallel[1].Start, "overlapping tests keep their own starts")
}

func TestRunnerScopeStepWindows(t *testing.T) {
	_, scope := feed(t, `{"Time":"2026-09-24T10:00:00Z","Action":"run","Package":"orders/e2e","Test":"TestA"}
{"Time":"2026-09-24T10:00:00.01Z","Action":"run","Package":"orders/e2e","Test":"TestA/create"}
{"Time":"2026-09-24T10:00:00.02Z","Action":"run","Package":"orders/e2e","Test":"TestA/create/nested"}
{"Time":"2026-09-24T10:00:00.03Z","Action":"pass","Package":"orders/e2e","Test":"TestA/create/nested"}
{"Time":"2026-09-24T10:00:00.04Z","Action":"pass","Package":"orders/e2e","Test":"TestA/create"}
{"Time":"2026-09-24T10:00:00.05Z","Action":"run","Package":"orders/e2e","Test":"TestA/delete"}
{"Time":"2026-09-24T10:00:00.09Z","Action":"pass","Package":"orders/e2e","Test":"TestA/delete"}
{"Time":"2026-09-24T10:00:00.1Z","Action":"pass","Package":"orders/e2e","Test":"TestA"}
`)
	sameWindows(t, []models.ScopeWindow{
		{Name: "create", Start: runnerT0.Add(20 * time.Millisecond), End: runnerT0.Add(30 * time.Millisecond)},
		{Name: "create", Start: runnerT0.Add(10 * time.Millisecond), End: runnerT0.Add(40 * time.Millisecond)},
		{Name: "delete", Start: runnerT0.Add(40 * time.Millisecond), End: runnerT0.Add(90 * time.Millisecond)},
	}, scope.stepWindows())

	_, plain := feed(t, plainSequential)
	var steps []string
	for _, w := range plain.stepWindows() {
		steps = append(steps, w.Name)
	}
	require.Equal(t, []string{"one", "two"}, steps)
	require.Nil(t, (*runnerScope)(nil).stepWindows())
}

func TestRunnerScopeRefusesATestThatRanTwice(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
	}{
		{
			name: "json, -count=2 or orders and orders_test both defining it",
			output: `{"Time":"2026-09-29T03:13:00.796794+05:30","Action":"run","Package":"example.com/dup/orders","Test":"TestX"}
{"Time":"2026-09-29T03:13:00.796875+05:30","Action":"run","Package":"example.com/dup/orders","Test":"TestX/step"}
{"Time":"2026-09-29T03:13:00.796884+05:30","Action":"pass","Package":"example.com/dup/orders","Test":"TestX/step","Elapsed":0}
{"Time":"2026-09-29T03:13:00.796888+05:30","Action":"pass","Package":"example.com/dup/orders","Test":"TestX","Elapsed":0}
{"Time":"2026-09-29T03:13:00.79689+05:30","Action":"run","Package":"example.com/dup/orders","Test":"TestX"}
{"Time":"2026-09-29T03:13:00.796904+05:30","Action":"pass","Package":"example.com/dup/orders","Test":"TestX","Elapsed":0}
`,
			want: "TestX ran 2 times in example.com/dup/orders; test names must be unique within a folder (check -count, or package orders and orders_test both defining it)",
		},
		{
			name:   "plain",
			output: "=== RUN   TestX\n--- PASS: TestX (0.00s)\n=== RUN   TestX\n--- PASS: TestX (0.00s)\n=== RUN   TestX\n--- PASS: TestX (0.00s)\n",
			want:   "TestX ran 3 times in one package; test names must be unique within a folder (check -count, or package <p> and <p>_test both defining it)",
		},
		{
			name:   "the same name in two packages is fine",
			output: `{"Action":"run","Package":"a","Test":"TestX"}` + "\n" + `{"Action":"pass","Package":"a","Test":"TestX"}` + "\n" + `{"Action":"run","Package":"b","Test":"TestX"}` + "\n" + `{"Action":"pass","Package":"b","Test":"TestX"}` + "\n",
		},
		{
			name:   "subtests of the same name under different tests are fine",
			output: plainSequential + "=== RUN   TestD\n=== RUN   TestD/one\n--- PASS: TestD (0.00s)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, scope := feed(t, tc.output)
			err := scope.repeated()
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.want)
		})
	}
	require.NoError(t, (*runnerScope)(nil).repeated())
}
