package mock

import (
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

func TestClassifyPutsEachStartupCallWithItsOwnStart(t *testing.T) {
	ms := func(n int) time.Time {
		return time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Millisecond)
	}
	suites := []models.SuiteSpan{
		{Dir: "/r/e2e/orders", Start: ms(0), End: ms(500)},
		{Dir: "/r/e2e/payments", Start: ms(600), End: ms(900)},
	}
	tests := []models.ScopeWindow{
		{Name: "orders.TestCreate", Start: ms(100), End: ms(200), PID: 10},
		{Name: "orders.TestReload", Start: ms(250), End: ms(450), PID: 10},
		{Name: "payments.TestPay", Start: ms(700), End: ms(800), PID: 20},
	}
	starts := []models.ScopeWindow{
		{App: true, Ref: "s1", Name: "shop#1", Dir: "/r/e2e/orders", Program: "shop", N: 1, Ready: ms(60), Worker: 10},
		{App: true, Ref: "s2", Name: "shop#1@orders.TestReload", Dir: "/r/e2e/orders", Program: "shop", Place: "orders.TestReload", N: 1, Ready: ms(320), Worker: 10},
		{App: true, Ref: "s3", Name: "shop#1", Dir: "/r/e2e/payments", Program: "shop", N: 1, Ready: ms(660), Worker: 20},
		{App: true, Ref: "s4", Name: "shop#2@orders.TestReload", Dir: "/r/e2e/orders", Program: "shop", Place: "orders.TestReload", N: 2, Worker: 10},
	}
	mocks := []capturedMock{
		{name: "config-inr", ts: ms(50), ref: "s1"},
		{name: "book", ts: ms(150), ref: "s1"},
		{name: "pen-old", ts: ms(260), ref: "s1"},
		{name: "config-usd", ts: ms(310), ref: "s2"},
		{name: "pen-new", ts: ms(330), ref: "s2"},
		{name: "pg-handshake", ts: ms(340), ref: "s2", boot: true},
		{name: "between", ts: ms(220), ref: "s1"},
		{name: "config-eur", ts: ms(650), ref: "s3"},
		{name: "worker-own", ts: ms(710), pid: 20},
		{name: "go-build", ts: ms(20), pid: 99},
	}
	p := classify(tests, starts, suites, mocks)
	boot := map[string][]string{}
	for _, b := range p.boots {
		var names []string
		for _, m := range b.Mocks {
			names = append(names, m.Name)
		}
		boot[b.Dir+"|"+b.Key] = names
	}
	expect := map[string][]string{
		"/r/e2e/orders|shop#1":                   {"config-inr", "between"},
		"/r/e2e/orders|shop#1@orders.TestReload": {"config-usd", "pg-handshake"},
		"/r/e2e/payments|shop#1":                 {"config-eur"},
		"/r/e2e/orders|shop#2@orders.TestReload": nil,
		"/r/e2e/orders|":                         {"go-build"},
	}
	for k, want := range expect {
		got, ok := boot[k]
		if !ok || len(got) != len(want) {
			t.Fatalf("boot %s = %v (present %v), want %v; all %v", k, got, ok, want, boot)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("boot %s = %v, want %v", k, got, want)
			}
		}
	}
	owner := map[string]string{}
	for test, entries := range p.tests {
		for _, e := range entries {
			owner[test+"|"+e.Name] = e.Start
		}
	}
	want := map[string]string{
		"orders.TestCreate|book":      "shop#1",
		"orders.TestReload|pen-old":   "shop#1",
		"orders.TestReload|pen-new":   "shop#1@orders.TestReload",
		"payments.TestPay|worker-own": "",
	}
	if len(owner) != len(want) {
		t.Fatalf("per-test = %v", owner)
	}
	for k, v := range want {
		if got, ok := owner[k]; !ok || got != v {
			t.Fatalf("per-test %s = %q (present %v), want %q", k, got, ok, v)
		}
	}
}

func TestTestAtUsesTheOnlyOpenTestWhenTheCallerHasNone(t *testing.T) {
	at := time.Date(2026, 10, 5, 10, 0, 1, 0, time.UTC)
	tests := []models.ScopeWindow{{Name: "login", Start: at.Add(-time.Second), End: at.Add(time.Second), PID: 30}}
	if got := testAt(tests, 10, nil, at); got != "login" {
		t.Fatalf("testAt = %q", got)
	}
	tests = append(tests, models.ScopeWindow{Name: "other", Start: at.Add(-time.Second), End: at.Add(time.Second), PID: 31})
	if got := testAt(tests, 10, nil, at); got != "" {
		t.Fatalf("two workers with open tests must be ambiguous, got %q", got)
	}
}

// Under the proven switch keploy mock record's mapping gives a per-test call
// to the test the Generic recorder states it is (models.CallOwner, asked by
// both with the same chain): a call the recorder states as one test's,
// unmarked, is that test's here, and one it marks manyTests is no test's
// here. Each case runs with the tests begun, and listed, in either order.
func TestTestAtNamesTheTestTheGenericRecorderStates(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(7000, 0)
	ms := func(f float64) time.Time { return t0.Add(time.Duration(f * float64(time.Millisecond))) }
	type test struct {
		name       string
		pid        uint32
		start, end float64
	}
	const other = 4000000000
	at := ms(19)
	for _, tc := range []struct {
		name  string
		tests []test
		chain []uint32 // the call's process, then its ancestors
		want  string   // the owner, or "" when no one can be told
	}{
		{"a child of B's worker (a browser it launched)", []test{{"A", other, 0, 19.1}, {"B", 100, 1, 30}}, []uint32{101, 100, 1}, "B"},
		{"B's worker itself", []test{{"A", other, 0, 19.1}, {"B", 100, 1, 30}}, []uint32{100, 1}, "B"},
		{"a grandchild of A's worker", []test{{"A", 100, 0, 25}, {"B", other, 1, 30}}, []uint32{102, 101, 100, 1}, "A"},
		{"the process known to neither worker", []test{{"A", other, 0, 19.1}, {"B", 100, 1, 30}}, []uint32{300, 1}, ""},
		{"no process known", []test{{"A", other, 0, 19.1}, {"B", 100, 1, 30}}, nil, ""},
		{"two tests with no worker", []test{{"A", 0, 0, 19.1}, {"B", 0, 1, 30}}, []uint32{300, 1}, ""},
		{"one worker's nested tests: the innermost", []test{{"A", 100, 0, 30}, {"B", 100, 1, 25}}, []uint32{101, 100, 1}, "B"},
		{"one worker's nested tests, the process not of it", []test{{"A", 100, 0, 30}, {"B", 100, 1, 25}}, []uint32{300, 1}, "B"},
		{"one worker's nested tests, no process known", []test{{"A", 100, 0, 30}, {"B", 100, 1, 25}}, nil, "B"},
		{"one worker's nested tests begun at one instant, the process not of it", []test{{"A", 100, 1, 30}, {"B", 100, 1, 25}}, []uint32{300, 1}, ""},
		{"two workers' tests, the process of neither", []test{{"A", 100, 0, 30}, {"B", 100, 1, 25}, {"C", 200, 2, 30}}, []uint32{300, 1}, ""},
		{"two workers both on the chain", []test{{"A", 100, 0, 30}, {"B", 101, 1, 25}}, []uint32{102, 101, 100, 1}, ""},
		{"two workers' tests begun at one instant, the process known to neither", []test{{"A", 100, 1, 19.1}, {"B", 200, 1, 30}}, []uint32{300, 1}, ""},
		{"two workers' tests begun at one instant, a child of B's worker", []test{{"A", 100, 1, 19.1}, {"B", 200, 1, 30}}, []uint32{201, 200, 1}, "B"},
		{"one worker's two tests begun at one instant", []test{{"A", 100, 1, 20}, {"B", 100, 1, 30}}, []uint32{100, 1}, ""},
		{"the only test, the process not its worker's", []test{{"A", 100, 0, 30}}, []uint32{300, 1}, "A"},
	} {
		for _, reverse := range []bool{false, true} {
			tests := tc.tests
			if reverse {
				tests = []test{}
				for i := len(tc.tests) - 1; i >= 0; i-- {
					tests = append(tests, tc.tests[i])
				}
			}
			mgr := syncMock.New(zap.NewNop())
			var windows []models.ScopeWindow
			var scopes []*syncMock.TestScope
			for _, x := range tests {
				scopes = append(scopes, mgr.BeginTestScope(ms(x.start), x.pid))
				windows = append(windows, models.ScopeWindow{Name: x.name, Start: ms(x.start), End: ms(x.end), PID: x.pid})
			}
			if open, ok := mgr.TestSpanAt(at, tc.chain); !ok || open.Final() || open.Shared() != (tc.want == "") {
				t.Fatalf("%s (reverse %v): while open the recorder got %+v %v, want shared %v", tc.name, reverse, open, ok, tc.want == "")
			}
			for i, x := range tests {
				scopes[i].End(ms(x.end))
			}
			span, ok := mgr.TestSpanAt(at, tc.chain)
			if !ok || !span.Final() {
				t.Fatalf("%s (reverse %v): the recorder got %+v %v, want final", tc.name, reverse, span, ok)
			}
			recorder := ""
			if !span.Shared() {
				for _, x := range tests {
					if span.Start.Equal(ms(x.start)) && span.End.Equal(ms(x.end)) {
						recorder = x.name
					}
				}
			}
			var pid uint32
			if len(tc.chain) > 0 {
				pid = tc.chain[0]
			}
			mapping := testAt(windows, pid, tc.chain, at)
			if recorder != tc.want || mapping != tc.want {
				t.Fatalf("%s (reverse %v): the recorder states %q, the mapping gives %q, want %q", tc.name, reverse, recorder, mapping, tc.want)
			}
			plan := classify(windows, nil, nil, []capturedMock{{name: "mock-0", kind: "Generic", ts: at, pid: pid, chain: tc.chain}})
			owner := ""
			for name, entries := range plan.tests {
				for _, e := range entries {
					if e.Name == "mock-0" {
						owner = name
					}
				}
			}
			if owner != tc.want {
				t.Fatalf("%s (reverse %v): classify gives the mock to %q, want %q (tests %v, boots %v)", tc.name, reverse, owner, tc.want, plan.tests, plan.boots)
			}
		}
	}
}

// Without the proven switch the mapping is as it was: a window whose PID is
// the call's own pid exactly owns it, the latest begun of several; with none,
// the latest begun of the windows holding the call, when they are all one
// worker's. No chain is read: a worker's child's call is no test's while
// another worker's test is open too.
func TestTestAtWithTheSwitchOffMatchesThePIDExactly(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	t0 := time.Unix(7000, 0)
	ms := func(f float64) time.Time { return t0.Add(time.Duration(f * float64(time.Millisecond))) }
	two := []models.ScopeWindow{{Name: "A", Start: t0, End: ms(19.1), PID: 4000000000}, {Name: "B", Start: ms(1), End: ms(30), PID: 100}}
	if got := testAt(two, 101, []uint32{101, 100, 1}, ms(19)); got != "" {
		t.Fatalf("a child of B's worker: %q, want none", got)
	}
	if got := testAt(two, 100, nil, ms(19)); got != "B" {
		t.Fatalf("B's worker: %q, want B", got)
	}
	nested := []models.ScopeWindow{{Name: "A", Start: t0, End: ms(30), PID: 100}, {Name: "B", Start: ms(1), End: ms(25), PID: 100}}
	if got := testAt(nested, 300, []uint32{300, 1}, ms(19)); got != "B" {
		t.Fatalf("one worker's nested tests, the process not of it: %q, want the innermost, B", got)
	}
}

// ownerOf is the test classify gives the mock name to, "" when none.
func ownerOf(plan bootPlan, name string) string {
	for test, entries := range plan.tests {
		for _, e := range entries {
			if e.Name == name {
				return test
			}
		}
	}
	return ""
}

// bootOf is the key of the boot classify puts the mock name in, and whether
// one holds it.
func bootOf(plan bootPlan, name string) (string, bool) {
	for _, b := range plan.boots {
		for _, e := range b.Mocks {
			if e.Name == name {
				return b.Key, true
			}
		}
	}
	return "", false
}

// Under the proven switch a call made while one worker's parent test and its
// subtest are both open, from a process that is not of that worker (an app
// started outside it) or one keploy could not read, is the subtest's, the
// innermost, for every parser, as the Generic recorder states it
// (models.CallOwner): no parser but Generic marks a pooled call, so a call
// the mapping gave to no test would leave the subtest judged with its call
// served from the pool. Two such tests begun at one instant give it to none.
func TestClassifyGivesOneWorkersSubtestACallFromOutsideTheWorker(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(7000, 0)
	ms := func(f float64) time.Time { return t0.Add(time.Duration(f * float64(time.Millisecond))) }
	for _, tc := range []struct {
		name  string
		pid   uint32
		chain []uint32
	}{
		{"a process not of the worker", 300, []uint32{300, 1}},
		{"its chain not read", 300, nil},
		{"no pid known", 0, nil},
	} {
		for _, kind := range []string{"Http", "Postgres", "Generic"} {
			for _, atOnce := range []bool{false, true} {
				sub := ms(1)
				want := "TestA/b"
				if atOnce {
					sub, want = t0, ""
				}
				nested := []models.ScopeWindow{{Name: "TestA", Start: t0, End: ms(30), PID: 100}, {Name: "TestA/b", Start: sub, End: ms(25), PID: 100}}
				plan := classify(nested, nil, nil, []capturedMock{{name: "mock-0", kind: kind, ts: ms(19), pid: tc.pid, chain: tc.chain}})
				if got := ownerOf(plan, "mock-0"); got != want {
					t.Errorf("%s, %s, begun at one instant %v: the call is %q's, want %q (boots %v)", tc.name, kind, atOnce, got, want, plan.boots)
				}
			}
		}
	}
}

// Under the proven switch the call of an app a worker started (a start the
// registry saw under that worker) is told by that worker too: when the chain
// read at a later accept no longer shows it (the app was reparented), or no
// chain was read, the start's worker still names its test while another
// worker's test is open beside it.
func TestClassifyTellsAStartsCallByItsWorker(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(7000, 0)
	ms := func(f float64) time.Time { return t0.Add(time.Duration(f * float64(time.Millisecond))) }
	tests := []models.ScopeWindow{{Name: "TestA", Start: t0, End: ms(30), PID: 100}, {Name: "TestB", Start: ms(1), End: ms(30), PID: 200}}
	starts := []models.ScopeWindow{{Name: "app#1", Ref: "s1", Worker: 100}}
	for _, tc := range []struct {
		name  string
		pid   uint32
		chain []uint32
	}{
		{"reparented: the chain no longer shows the worker", 300, []uint32{300, 1}},
		{"no chain read", 300, nil},
		{"no pid known", 0, nil},
		{"the chain under the worker", 300, []uint32{300, 100, 1}},
	} {
		plan := classify(tests, starts, nil, []capturedMock{{name: "mock-0", kind: "Http", ts: ms(19), pid: tc.pid, chain: tc.chain, ref: "s1"}})
		if got := ownerOf(plan, "mock-0"); got != "TestA" {
			t.Errorf("%s: the start's call is %q's, want its worker's TestA (boots %v)", tc.name, got, plan.boots)
		}
	}
}

// Under the proven switch a protocol-phase mock (models.Mock.IsProtocolPhase:
// the Generic recorder's session exchange, marked or not) is the pool's,
// never a test's, in the mapping classify writes as in correlateScopes: a
// test that listed it would own a call replay does not assert of it. From
// the worker it goes to the set's pool; from an app the test started, to
// that app's boot. A data-plane mock beside it is still the test's.
func TestClassifyKeepsProtocolPhaseMocksOutOfTestMappings(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(7000, 0)
	ms := func(f float64) time.Time { return t0.Add(time.Duration(f * float64(time.Millisecond))) }
	tests := []models.ScopeWindow{{Name: "p::TestA", Start: t0, End: ms(10), PID: 100}, {Name: "p::TestB", Start: ms(11), End: ms(30), PID: 100}}
	starts := []models.ScopeWindow{{Name: "cli#1@p::TestB", Ref: "s1", Worker: 100, Place: "p::TestB", Ready: ms(12)}}
	mocks := []capturedMock{
		{name: "worker-session", kind: "Generic", ts: ms(19), pid: 100, chain: []uint32{100, 1}, boot: true, protocol: true},
		{name: "worker-startup", kind: "Postgres", ts: ms(19), pid: 100, chain: []uint32{100, 1}, protocol: true},
		{name: "cli-session", kind: "Generic", ts: ms(20), pid: 101, chain: []uint32{101, 100, 1}, boot: true, protocol: true, ref: "s1", start: "cli#1@p::TestB"},
		{name: "cli-startup", kind: "Postgres", ts: ms(20), pid: 101, chain: []uint32{101, 100, 1}, protocol: true, ref: "s1", start: "cli#1@p::TestB"},
		{name: "worker-query", kind: "Postgres", ts: ms(21), pid: 100, chain: []uint32{100, 1}},
		{name: "cli-query", kind: "Postgres", ts: ms(22), pid: 101, chain: []uint32{101, 100, 1}, ref: "s1", start: "cli#1@p::TestB"},
	}
	plan := classify(tests, starts, nil, mocks)
	for _, tc := range []struct{ mock, test, boot string }{
		{"worker-session", "", ""},
		{"worker-startup", "", ""},
		{"cli-session", "", "cli#1@p::TestB"},
		{"cli-startup", "", "cli#1@p::TestB"},
		{"worker-query", "p::TestB", "-"},
		{"cli-query", "p::TestB", "-"},
	} {
		test := ownerOf(plan, tc.mock)
		boot, inBoot := bootOf(plan, tc.mock)
		if test != tc.test || (tc.boot == "-") == inBoot || (inBoot && boot != tc.boot) {
			t.Errorf("%s: in test %q, boot %q (%v); want test %q, boot %q", tc.mock, test, boot, inBoot, tc.test, tc.boot)
		}
	}
	for _, mk := range plan.perTest {
		if mk.protocol {
			t.Errorf("a protocol-phase mock %s is a test's call (perTest)", mk.name)
		}
	}
}
