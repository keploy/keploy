package mock

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestParseRunnerLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want scopeEvent
	}{
		{"json run is a begin, qualified by package", `{"Time":"2026-09-24T10:00:00Z","Action":"run","Package":"github.com/acme/shop/cart","Test":"TestAdd"}`,
			scopeEvent{kind: scopeBegin, pkg: "github.com/acme/shop/cart", test: "TestAdd"}},
		{"json pass is an end", `{"Action":"pass","Package":"cart","Test":"TestAdd","Elapsed":0.01}`, scopeEvent{kind: scopeEnd, pkg: "cart", test: "TestAdd"}},
		{"json fail is an end", `{"Action":"fail","Package":"cart","Test":"TestAdd"}`, scopeEvent{kind: scopeEnd, pkg: "cart", test: "TestAdd"}},
		{"json skip is an end", `{"Action":"skip","Package":"cart","Test":"TestAdd/empty"}`, scopeEvent{kind: scopeEnd, pkg: "cart", test: "TestAdd/empty"}},
		{"json subtest keeps its full path", `{"Action":"run","Package":"cart","Test":"TestAdd/two_items"}`, scopeEvent{kind: scopeBegin, pkg: "cart", test: "TestAdd/two_items"}},
		{"json without a package uses the bare test", `{"Action":"run","Test":"TestAdd"}`, scopeEvent{kind: scopeBegin, test: "TestAdd"}},
		{"json pause is a pause", `{"Action":"pause","Package":"cart","Test":"TestAdd"}`, scopeEvent{kind: scopePause, pkg: "cart", test: "TestAdd"}},
		{"json output is ignored", `{"Action":"output","Package":"cart","Test":"TestAdd","Output":"=== RUN   TestAdd\n"}`, scopeEvent{}},
		{"json cont is ignored", `{"Action":"cont","Package":"cart","Test":"TestAdd"}`, scopeEvent{}},
		{"json package event without a test is ignored", `{"Action":"pass","Package":"cart","Elapsed":0.2}`, scopeEvent{}},
		{"broken json is ignored", `{"Action":"run","Test":`, scopeEvent{}},
		{"plain run", "=== RUN   TestAdd", scopeEvent{kind: scopeBegin, test: "TestAdd"}},
		{"plain indented subtest run", "    === RUN   TestAdd/two_items", scopeEvent{kind: scopeBegin, test: "TestAdd/two_items"}},
		{"plain pass with duration", "--- PASS: TestAdd (0.01s)", scopeEvent{kind: scopeEnd, test: "TestAdd"}},
		{"plain indented subtest pass", "    --- PASS: TestAdd/two_items (0.00s)", scopeEvent{kind: scopeEnd, test: "TestAdd/two_items"}},
		{"plain fail", "--- FAIL: TestAdd (0.01s)", scopeEvent{kind: scopeEnd, test: "TestAdd"}},
		{"plain skip", "        --- SKIP: TestAdd/empty (0.00s)", scopeEvent{kind: scopeEnd, test: "TestAdd/empty"}},
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
		{"colour codes are dropped", "\x1b[36mtests-1  | \x1b[0m--- PASS: TestAdd (0.00s)", scopeEvent{kind: scopeEnd, test: "TestAdd"}},
		{"a pipe inside a log line is not a prefix", "    cart_test.go:12: got a | b", scopeEvent{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, parseRunnerLine(tc.line))
		})
	}
}

func TestScopeEventName(t *testing.T) {
	require.Equal(t, "cart.TestAdd/x", scopeEvent{pkg: "cart", test: "TestAdd/x"}.name())
	require.Equal(t, "TestAdd", scopeEvent{test: "TestAdd"}.name())
}

// fakeMarker records the scope calls in order.
type fakeMarker struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeMarker) BeginScope(_ context.Context, name string, pid int) error {
	return f.add("begin", name, pid)
}

func (f *fakeMarker) EndScope(_ context.Context, name string, pid int) error {
	return f.add("end", name, pid)
}

func (f *fakeMarker) add(mark, name string, pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pid != 0 {
		f.calls = append(f.calls, "unexpected pid")
	}
	f.calls = append(f.calls, mark+" "+name)
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

func TestRunnerScopeIgnoresUnrelatedOutput(t *testing.T) {
	marker, scope := feed(t, "collected 3 items\n\ntest_cart.py::test_add PASSED\n=== something else\n")
	require.Empty(t, marker.calls)
	require.Empty(t, scope.overlapping())
}

func TestFirstFew(t *testing.T) {
	require.Equal(t, "a, b", firstFew([]string{"a", "b"}, 10))
	require.Equal(t, "a, b and 3 more", firstFew(strings.Split("a b c d e", " "), 2))
}

func TestNilRunnerScopeIsOff(t *testing.T) {
	var scope *runnerScope
	require.Nil(t, scope.writer())
	require.Nil(t, scope.overlapping())
}
