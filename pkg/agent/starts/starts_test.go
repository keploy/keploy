package starts

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

type fake struct {
	parent  map[uint32]uint32
	born    map[uint32]time.Time
	program map[uint32]string
}

func (f *fake) Parent(pid uint32) (uint32, bool) { p, ok := f.parent[pid]; return p, ok }

func (f *fake) Birth(pid uint32) (time.Time, bool) { b, ok := f.born[pid]; return b, ok }

func (f *fake) Program(pid uint32) string { return f.program[pid] }

func (f *fake) spawn(pid, parent uint32, at time.Time, program string) {
	f.parent[pid] = parent
	f.born[pid] = at
	f.program[pid] = program
}

func newFake() *fake {
	return &fake{parent: map[uint32]uint32{}, born: map[uint32]time.Time{}, program: map[uint32]string{}}
}

var t0 = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

func TestKeysForSuiteStartRestartAndSecondPackage(t *testing.T) {
	f := newFake()
	r := New(f, 0)
	f.spawn(100, 1, at(0), "/tmp/orders.test")
	r.Begin(100, "/repo/e2e/orders", "/repo/e2e/orders", true, at(10))
	f.spawn(101, 100, at(50), "/tmp/t2demo-shop-48211")
	r.Note("1", 101, at(60))
	r.Ready(101, 8080, at(70))
	r.Begin(100, "orders.TestReload", "/repo/e2e/orders", false, at(100))
	r.Note("2", 101, at(110))
	f.spawn(102, 100, at(150), "/tmp/t2demo-shop-48211")
	r.Note("3", 102, at(160))
	r.Ready(102, 8080, at(170))
	r.End(100, "orders.TestReload", false, at(200))
	r.End(100, "/repo/e2e/orders", true, at(300))

	f.spawn(200, 1, at(400), "/tmp/payments.test")
	r.Begin(200, "/repo/e2e/payments", "/repo/e2e/payments", true, at(410))
	f.spawn(201, 200, at(450), "/tmp/t2demo-shop-51000")
	r.Note("4", 201, at(460))

	got := map[string]string{}
	for _, s := range r.List() {
		got[s.Dir+"|"+s.Key] = s.Ready.String()
	}
	want := []string{
		"/repo/e2e/orders|t2demo-shop#1",
		"/repo/e2e/orders|t2demo-shop#1@orders.TestReload",
		"/repo/e2e/payments|t2demo-shop#1",
	}
	if len(got) != len(want) {
		t.Fatalf("starts = %v", got)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("missing %s in %v", k, got)
		}
	}
	m := &models.Mock{ConnectionID: "2"}
	r.Stamp(m)
	if m.Start != "t2demo-shop#1" || m.SourcePID != 101 {
		t.Fatalf("old shop's call in the test must keep its start, got %q %d", m.Start, m.SourcePID)
	}
	m = &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{"connID": "3"}}}
	r.Stamp(m)
	if m.Start != "t2demo-shop#1@orders.TestReload" {
		t.Fatalf("restarted shop = %q", m.Start)
	}
}

func TestWrapperRestartIsANewStartAndPreforkWorkersStayWithTheirMaster(t *testing.T) {
	f := newFake()
	r := New(f, 0)
	f.spawn(100, 1, at(0), "orders.test")
	r.Begin(100, "e2e/orders", "/r/e2e/orders", true, at(10))
	f.spawn(110, 100, at(20), "sh")
	f.spawn(111, 110, at(30), "shop")
	f.spawn(112, 111, at(40), "shop")
	r.Note("a", 112, at(50))
	r.Begin(100, "orders.TestA", "/r/e2e/orders", false, at(100))
	r.Note("b", 111, at(110))
	f.spawn(113, 110, at(150), "shop")
	r.Note("c", 113, at(160))
	keys := map[uint32]string{}
	for _, c := range []struct {
		conn string
		pid  uint32
	}{{"a", 112}, {"b", 111}, {"c", 113}} {
		m := &models.Mock{ConnectionID: c.conn}
		r.Stamp(m)
		keys[c.pid] = m.Start
	}
	if keys[112] != "shop#1" || keys[111] != "shop#1" {
		t.Fatalf("worker and master must share the suite start: %v", keys)
	}
	if keys[113] != "shop#1@orders.TestA" {
		t.Fatalf("a restart under a long-lived wrapper must be a new start: %v", keys)
	}
}

func TestOrdinalsRestartEachTimeAPlaceOpens(t *testing.T) {
	f := newFake()
	r := New(f, 0)
	f.spawn(100, 1, at(0), "orders.test")
	r.Begin(100, "e2e/orders", "/r/e2e/orders", true, at(1))
	for round, base := range []int{100, 300} {
		r.Begin(100, "orders.TestReload", "/r/e2e/orders", false, at(base))
		pid := uint32(200 + round)
		f.spawn(pid, 100, at(base+10), "shop")
		r.Note("", pid, at(base+20))
		r.End(100, "orders.TestReload", false, at(base+50))
	}
	f.spawn(250, 100, at(380), "shop")
	r.Note("", 250, at(390))
	var keys []string
	for _, s := range r.List() {
		keys = append(keys, s.Key)
	}
	want := []string{"shop#1@orders.TestReload", "shop#1@orders.TestReload", "shop#1@after:orders.TestReload"}
	if len(keys) != 3 || keys[0] != want[0] || keys[1] != want[1] || keys[2] != want[2] {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
}

func TestReplayBindsEachStartToItsOwnBootAndHidesTheOthers(t *testing.T) {
	f := newFake()
	r := New(f, 0)
	r.SetTable("/r", map[string]models.SetTable{
		"e2e/orders": {
			Boots: map[string][]string{
				"shop#1":                   {"boot-inr"},
				"shop#1@orders.TestReload": {"boot-usd"},
			},
			Tests: map[string][]models.Owned{
				"orders.TestReload": {{Name: "pen-old", Start: "shop#1"}, {Name: "pen-new", Start: "shop#1@orders.TestReload"}},
				"orders.TestCreate": {{Name: "book", Start: "shop#1"}},
			},
		},
		"e2e/payments": {Boots: map[string][]string{"shop#1": {"boot-eur"}}},
	})
	f.spawn(100, 1, at(0), "orders.test")
	r.Begin(100, "e2e/orders", "/r/e2e/orders", true, at(10))
	f.spawn(101, 100, at(20), "shop")
	rank, universe, ok := r.View(101, at(30))
	if !ok || rank["boot-inr"] != 2 || hasAny(rank, "boot-usd", "boot-eur") {
		t.Fatalf("TestMain start view = %v", rank)
	}
	r.Begin(100, "orders.TestReload", "/r/e2e/orders", false, at(100))
	f.spawn(102, 100, at(150), "shop")
	rank, _, _ = r.View(102, at(160))
	if rank["boot-usd"] != 2 || hasAny(rank, "boot-inr", "boot-eur") {
		t.Fatalf("restart view = %v", rank)
	}
	if rank["pen-new"] != 1 || rank["pen-old"] != 3 {
		t.Fatalf("own per-test mocks must come before another start's: %v", rank)
	}
	if rank["book"] != 0 {
		t.Fatalf("another test's mock owned by another start must stay hidden: %v", rank)
	}
	if _, ok := universe["boot-eur"]; !ok {
		t.Fatal("the universe must list every recorded name so unlisted ones are hidden")
	}
	rank, _, _ = r.View(101, at(170))
	if rank["pen-old"] != 1 || rank["boot-inr"] != 2 || hasAny(rank, "book") {
		t.Fatalf("old shop during the test sees only this test's mocks and its own boot: %v", rank)
	}
}

func TestAnUnrecordedStartFallsBackToTheSuiteStartOfTheSameProgram(t *testing.T) {
	f := newFake()
	r := New(f, 0)
	r.SetTable("/r", map[string]models.SetTable{"e2e/orders": {Boots: map[string][]string{"shop#1": {"boot"}}}})
	f.spawn(100, 1, at(0), "orders.test")
	r.Begin(100, "e2e/orders", "/r/e2e/orders", true, at(10))
	r.Begin(100, "orders.TestNew", "/r/e2e/orders", false, at(20))
	f.spawn(101, 100, at(30), "shop")
	rank, _, _ := r.View(101, at(40))
	if rank["boot"] != 2 {
		t.Fatalf("view = %v", rank)
	}
}

func TestSetsMatchByPathEndingWhenTheCheckoutMoved(t *testing.T) {
	f := newFake()
	r := New(f, 0)
	r.SetTable("/elsewhere", map[string]models.SetTable{"e2e/orders": {Boots: map[string][]string{"shop#1": {"b"}}}})
	f.spawn(100, 1, at(0), "orders.test")
	r.Begin(100, "e2e/orders", "/ci/build/repo/e2e/orders", true, at(10))
	f.spawn(101, 100, at(20), "shop")
	if rank, _, ok := r.View(101, at(30)); !ok || rank["b"] != 2 {
		t.Fatalf("view = %v %v", rank, ok)
	}
}

func TestOnlyOpenTestInTheRunServesAnAppUnderTheRunner(t *testing.T) {
	f := newFake()
	r := New(f, 0)
	r.SetTable("/r", map[string]models.SetTable{"web": {
		Boots: map[string][]string{"server#1": {"boot"}},
		Tests: map[string][]models.Owned{"login": {{Name: "auth", Start: "server#1"}}},
	}})
	f.spawn(10, 1, at(0), "node")
	r.Begin(10, "web", "/r/web", true, at(1))
	f.spawn(11, 10, at(5), "server")
	f.spawn(20, 10, at(6), "node")
	r.Begin(20, "login", "/r/web", false, at(50))
	rank, _, _ := r.View(11, at(60))
	if rank["auth"] != 1 {
		t.Fatalf("the app under the runner must see the test open in the worker: %v", rank)
	}
}

func TestNamesDropDigits(t *testing.T) {
	for in, want := range map[string]string{"/tmp/t2demo-shop-48211": "t2demo-shop", "shop": "shop", "/x/123": "app", "": "app"} {
		if got := Name(in); got != want {
			t.Fatalf("Name(%q) = %q, want %q", in, got, want)
		}
	}
}

func hasAny(rank map[string]int, names ...string) bool {
	for _, n := range names {
		if _, ok := rank[n]; ok {
			return true
		}
	}
	return false
}

func TestStampWritesTheDestinationOfTheCall(t *testing.T) {
	r := New(newFake(), 0)
	r.Dest("7", "127.0.0.1:9000")
	m := &models.Mock{ConnectionID: "7"}
	r.Stamp(m)
	if got := m.Spec.Metadata["destAddr"]; got != "127.0.0.1:9000" {
		t.Fatalf("destAddr = %q", got)
	}
	kept := &models.Mock{ConnectionID: "7", Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "db:3306"}}}
	r.Stamp(kept)
	if got := kept.Spec.Metadata["destAddr"]; got != "db:3306" {
		t.Fatalf("a recorder's own destAddr must stay, got %q", got)
	}
}

func TestUnmarkedProcessesInTheTreeAreDependenciesOnceAnAppIsMarked(t *testing.T) {
	f := newFake()
	r := New(f, 0)
	f.spawn(100, 1, at(0), "orders.test")
	r.Begin(100, "/r/e2e/orders", "/r/e2e/orders", true, at(10))
	f.spawn(101, 100, at(20), "pricing")
	f.spawn(102, 100, at(30), "shop")
	f.spawn(103, 102, at(35), "shop-worker")
	if r.Dependency(101) {
		t.Fatal("before any app is marked, nothing is a dependency")
	}
	r.Mark(102, at(31))
	if !r.Dependency(101) {
		t.Fatal("an unmarked process the tests started is a dependency")
	}
	if r.Dependency(102) || r.Dependency(103) {
		t.Fatal("the marked app and its children are the app")
	}
	r.Reset()
	if r.Dependency(101) {
		t.Fatal("a new run starts unmarked")
	}
}

func TestOnMarkRunsOnceWhenTheFirstAppIsMarked(t *testing.T) {
	f := newFake()
	r := New(f, 0)
	f.spawn(100, 1, at(0), "orders.test")
	r.Begin(100, "/r/e2e/orders", "/r/e2e/orders", true, at(10))
	f.spawn(102, 100, at(30), "shop")
	calls := make(chan struct{}, 4)
	r.OnMark(func() { calls <- struct{}{} })
	r.Mark(102, at(31))
	r.Ready(102, 8080, at(40))
	<-calls
	select {
	case <-calls:
		t.Fatal("the hook runs only for the first mark")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStartIsTheRecordedStartOfTheMarksSet(t *testing.T) {
	r := New(newFake(), time.Second)
	r.SetTable("/r", map[string]models.SetTable{"a": {Start: t0}, "b": {}})
	r.Begin(7, "s", "/r/a", true, at(0))
	if set, got := r.Start(7, ""); set != "a" || !got.Equal(t0) {
		t.Fatalf("worker's set start = %q %v", set, got)
	}
	if set, got := r.Start(0, "/r/a"); set != "a" || !got.Equal(t0) {
		t.Fatalf("dir's set start = %q %v", set, got)
	}
	if set, got := r.Start(0, "/r/b"); set != "b" || !got.IsZero() {
		t.Fatalf("set without a start = %q %v", set, got)
	}
	if set, got := r.Start(9, "/elsewhere"); set != "" || !got.IsZero() {
		t.Fatalf("no set = %q %v", set, got)
	}
}

func TestLiveNamesTheSetsWithASuiteOrATestOpen(t *testing.T) {
	r := New(newFake(), time.Second)
	r.SetTable("/r", map[string]models.SetTable{"a": {}, "b": {}, "c": {}})
	if got := r.Live(); len(got) != 0 {
		t.Fatalf("live before any mark: %v", got)
	}
	r.Begin(7, "/r/a", "/r/a", true, at(0))
	r.Begin(8, "t1", "/r/b", false, at(1))
	r.Begin(9, "t2", "/elsewhere", false, at(1))
	if got := r.Live(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("live = %v", got)
	}
	r.End(8, "t1", false, at(2))
	if got := r.Live(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("a set whose only test ended is still live: %v", got)
	}
	r.Begin(7, "t3", "", false, at(3))
	r.End(7, "t3", false, at(4))
	if got := r.Live(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("a set whose suite is open is not live: %v", got)
	}
	r.End(7, "/r/a", true, at(5))
	if got := r.Live(); len(got) != 0 {
		t.Fatalf("live after every suite ended: %v", got)
	}
}

func TestLiveLeavesOutASetWhoseRunnerDiedMidSuite(t *testing.T) {
	f := newFake()
	r := New(f, time.Second)
	r.SetTable("/r", map[string]models.SetTable{"a": {}, "b": {}})
	f.spawn(7, 1, at(0), "/tmp/a.test")
	r.Begin(7, "/r/a", "/r/a", true, at(1))
	r.Begin(8, "/r/b", "/r/b", true, at(1))
	if got := r.Live(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("live = %v", got)
	}
	delete(f.born, 7)
	if got := r.Live(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("a runner that died without ending its suite still holds its set: %v", got)
	}
	f.spawn(7, 1, at(5), "/tmp/other")
	if got := r.Live(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("a new process with the dead runner's pid holds its set: %v", got)
	}
}

func caseTable() map[string]models.SetTable {
	return map[string]models.SetTable{"e2e": {
		Tests: map[string][]models.Owned{"T": {{Name: "m1"}, {Name: "m2"}, {Name: "m3"}, {Name: "fx"}}, "U": {{Name: "u1"}}},
		Cases: map[string][]models.Case{"T": {
			{Name: "test-1", Method: "GET", URL: "http://localhost:8080/a", Mocks: []string{"m1"}},
			{Name: "test-2", Method: "GET", URL: "http://localhost:8080/a", Mocks: []string{"m2"}},
			{Name: "test-3", Method: "POST", URL: "http://localhost:8080/b/42", Mocks: []string{"m3"}},
		}},
	}}
}

func seen(t *testing.T, r *Registry, pid uint32) []string {
	t.Helper()
	rank, _, ok := r.View(pid, at(20))
	if !ok {
		t.Fatal("no view")
	}
	var out []string
	for n := range rank {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func sees(t *testing.T, r *Registry, pid uint32, what string, want ...string) {
	t.Helper()
	if got := seen(t, r, pid); !slices.Equal(got, want) {
		t.Fatalf("%s: sees %v, want %v", what, got, want)
	}
}

func oneTest(sets map[string]models.SetTable) (*fake, *Registry) {
	f := newFake()
	r := New(f, 0)
	f.spawn(100, 1, at(0), "/tmp/e2e.test")
	r.Begin(100, "/repo/e2e", "/repo/e2e", true, at(1))
	f.spawn(101, 100, at(2), "/tmp/shop")
	r.SetTable("/repo", sets)
	r.Begin(100, "T", "/repo/e2e", false, at(10))
	return f, r
}

func TestACallSeesOnlyTheMocksOfTheCaseInFlight(t *testing.T) {
	_, r := oneTest(caseTable())
	sees(t, r, 101, "before any request is paired", "fx", "m1", "m2", "m3")
	end := r.Exchange(0, "GET", "/a")
	sees(t, r, 101, "first GET /a", "fx", "m1")
	end()
	end()
	sees(t, r, 101, "between the first and second case", "fx", "m1", "m2")
	end = r.Exchange(0, "GET", "/a")
	sees(t, r, 101, "second GET /a", "fx", "m2")
	end()
	sees(t, r, 101, "between the second and third case", "fx", "m2", "m3")
	end = r.Exchange(0, "POST", "/b/43/")
	sees(t, r, 101, "POST /b by its path", "fx", "m3")
	end()
	sees(t, r, 101, "after the last case", "fx", "m3")
}

func TestARequestNoCaseRecordedSeesTheWholeTest(t *testing.T) {
	_, r := oneTest(caseTable())
	for _, c := range []struct{ method, url string }{{"GET", "/a"}, {"GET", "/a"}, {"POST", "/b/1"}} {
		r.Exchange(0, c.method, c.url)()
	}
	end := r.Exchange(0, "GET", "/a")
	sees(t, r, 101, "a third GET /a", "fx", "m1", "m2", "m3")
	end()
	end = r.Exchange(0, "GET", "/c")
	sees(t, r, 101, "GET /c", "fx", "m1", "m2", "m3")
	end()
	sees(t, r, 101, "after it", "fx", "m3")
}

func TestWithoutCasesTheWholeTestIsServed(t *testing.T) {
	_, r := oneTest(map[string]models.SetTable{"e2e": {
		Tests: map[string][]models.Owned{"T": {{Name: "m1"}, {Name: "fx"}}},
	}})
	end := r.Exchange(0, "GET", "/a")
	defer end()
	sees(t, r, 101, "a test with no cases", "fx", "m1")
}

func TestATestWithoutCasesInASetWithCasesIsServedWhole(t *testing.T) {
	_, r := oneTest(caseTable())
	r.End(100, "T", false, at(11))
	r.Begin(100, "U", "/repo/e2e", false, at(12))
	end := r.Exchange(0, "GET", "/a")
	defer end()
	sees(t, r, 101, "test U", "u1")
}

func twoPackages() (*fake, *Registry) {
	f, r := oneTest(caseTable())
	r.Ready(101, 8080, at(3))
	f.spawn(200, 1, at(0), "/tmp/other.test")
	r.Begin(200, "/repo/e2e", "/repo/e2e", true, at(1))
	f.spawn(201, 200, at(2), "/tmp/shop")
	r.Ready(201, 9090, at(3))
	r.Begin(200, "T", "/repo/e2e", false, at(10))
	return f, r
}

func TestTwoOpenTestsPairEachRequestByItsAppsPort(t *testing.T) {
	_, r := twoPackages()
	end := r.Exchange(9090, "GET", "/a")
	sees(t, r, 201, "second package's GET /a", "fx", "m1")
	sees(t, r, 101, "first package, nothing paired yet", "fx", "m1", "m2", "m3")
	end()
	end = r.Exchange(8080, "POST", "/b/7")
	sees(t, r, 101, "first package's POST", "fx", "m3")
	end()
	end = r.Exchange(0, "GET", "/a")
	sees(t, r, 101, "a request no app port places", "fx", "m1", "m2", "m3")
	end()
	sees(t, r, 101, "the rest of the test, after a request it may have missed", "fx", "m1", "m2", "m3")
}

func TestARequestToAnAppBetweenTestsLeavesOtherTestsCasesAlone(t *testing.T) {
	_, r := twoPackages()
	r.End(100, "T", false, at(11))
	end := r.Exchange(8080, "GET", "/a")
	sees(t, r, 201, "the other package while it is in flight", "fx", "m1", "m2", "m3")
	end()
	end = r.Exchange(9090, "GET", "/a")
	defer end()
	sees(t, r, 201, "the other package's own first GET /a", "fx", "m1")
}

func TestAPlacelessRequestFromBeforeAResetDoesNotEndOneAfterIt(t *testing.T) {
	f, r := twoPackages()
	old := r.Exchange(0, "GET", "/a")
	r.Reset()
	r.Begin(100, "/repo/e2e", "/repo/e2e", true, at(1))
	r.Begin(200, "/repo/e2e", "/repo/e2e", true, at(1))
	r.SetTable("/repo", caseTable())
	r.Begin(100, "T", "/repo/e2e", false, at(10))
	r.Begin(200, "T", "/repo/e2e", false, at(10))
	f.spawn(102, 100, at(12), "/tmp/shop")
	r.Exchange(0, "GET", "/a")()
	end := r.Exchange(0, "GET", "/a")
	defer end()
	old()
	sees(t, r, 102, "the new run's placeless request", "fx", "m1", "m2", "m3")
}

func TestATestWhoseProcessDiedDoesNotBlockPairing(t *testing.T) {
	f, r := oneTest(caseTable())
	f.spawn(300, 1, at(0), "/tmp/dead.test")
	r.Begin(300, "/repo/e2e", "/repo/e2e", true, at(1))
	r.Begin(300, "U", "/repo/e2e", false, at(5))
	delete(f.born, 300)
	end := r.Exchange(0, "GET", "/a")
	defer end()
	sees(t, r, 101, "GET /a", "fx", "m1")
}

func TestASiblingsMockInAnotherTestsCaseStaysVisible(t *testing.T) {
	_, r := oneTest(map[string]models.SetTable{"e2e": {
		Tests: map[string][]models.Owned{"T": {{Name: "x"}, {Name: "t1"}, {Name: "t2"}}, "T/a": {{Name: "a1"}}},
		Cases: map[string][]models.Case{
			"T":   {{Name: "test-1", Method: "GET", URL: "/t", Mocks: []string{"t1"}}, {Name: "test-3", Method: "GET", URL: "/u", Mocks: []string{"t2"}}},
			"T/a": {{Name: "test-2", Method: "GET", URL: "/a", Mocks: []string{"a1", "x"}}},
		},
	}})
	end := r.Exchange(0, "GET", "/u")
	defer end()
	sees(t, r, 101, "T's second case", "t2", "x")
}

func TestPickCaseKeys(t *testing.T) {
	cases := []models.Case{
		{Method: "GET", URL: "http://h/a%2Fb"},
		{Method: "GET", URL: "http://h/u/01HZX3N4QK8V9E2R5T7Y6W1M0P"},
		{Method: "GET", URL: "http://h/q?x=1&y=2"},
	}
	for _, c := range []struct {
		method, url string
		want        int
	}{
		{"GET", "/a/b", -1},
		{"GET", "/a%2Fb", 0},
		{"get", "/u/01J0000000000000000000000Z/", 1},
		{"GET", "/q?y=2&x=1", 2},
		{"POST", "/q?x=1&y=2", -1},
	} {
		if got := pickCase(bookOf(cases, nil), make([]bool, len(cases)), c.method, c.url); got != c.want {
			t.Errorf("%s %s pairs with %d, want %d", c.method, c.url, got, c.want)
		}
	}
}

func TestATestWhoseRequestsNeverPairIsServedWhole(t *testing.T) {
	_, r := oneTest(caseTable())
	r.Exchange(0, "GET", "/zzz")()
	sees(t, r, 101, "after a request no case recorded", "fx", "m1", "m2", "m3")
}

func TestAnEndedCaseStaysServedWhileTheNextIsInFlight(t *testing.T) {
	_, r := oneTest(caseTable())
	first := r.Exchange(0, "GET", "/a")
	third := r.Exchange(0, "POST", "/b/1")
	first()
	sees(t, r, 101, "the first case ended, the third in flight", "fx", "m1", "m3")
	third()
}

func TestBetweenCasesTheNextCaseAfterTheLastIsServed(t *testing.T) {
	_, r := oneTest(caseTable())
	r.Exchange(0, "GET", "/a")()
	r.Exchange(0, "GET", "/a")()
	r.Exchange(0, "POST", "/b/1")()
	sees(t, r, 101, "after every case", "fx", "m3")
	_, r = oneTest(caseTable())
	r.Exchange(0, "POST", "/b/1")()
	sees(t, r, 101, "after the third case, the first not served", "fx", "m1", "m3")
}

func TestAnAppWhoseRunnerIsBetweenTestsIsNotNarrowedByAnotherPackagesTest(t *testing.T) {
	f := newFake()
	r := New(f, 0)
	f.spawn(100, 1, at(0), "/tmp/a.test")
	r.Begin(100, "/repo/a", "/repo/a", true, at(1))
	f.spawn(101, 100, at(2), "/tmp/shop")
	f.spawn(200, 1, at(0), "/tmp/b.test")
	r.Begin(200, "/repo/b", "/repo/b", true, at(1))
	f.spawn(201, 200, at(2), "/tmp/shop")
	r.SetTable("/repo", map[string]models.SetTable{
		"a": {Tests: map[string][]models.Owned{"T": {{Name: "a1"}, {Name: "a2"}}},
			Cases: map[string][]models.Case{"T": {{Name: "test-1", Method: "GET", URL: "/x", Mocks: []string{"a1"}}}}},
		"b": {Tests: map[string][]models.Owned{"T": {{Name: "b1"}, {Name: "b2"}, {Name: "b3"}}},
			Cases: map[string][]models.Case{"T": {
				{Name: "test-1", Method: "GET", URL: "/x", Mocks: []string{"b1"}},
				{Name: "test-2", Method: "GET", URL: "/y", Mocks: []string{"b2"}},
				{Name: "test-3", Method: "GET", URL: "/z", Mocks: []string{"b3"}},
			}}},
	})
	r.Ready(201, 9090, at(3))
	r.Begin(200, "T", "/repo/b", false, at(10))
	end := r.Exchange(9090, "GET", "/z")
	defer end()
	sees(t, r, 201, "b's third case", "b3")
	sees(t, r, 101, "a's app between a's tests", "a1", "a2")
}

func TestRequestsTheHookCannotTellApartShareTheirMocks(t *testing.T) {
	_, r := oneTest(map[string]models.SetTable{"e2e": {
		Tests: map[string][]models.Owned{"T": {{Name: "m0"}, {Name: "m1"}, {Name: "m2"}, {Name: "g1"}, {Name: "g2"}}},
		Cases: map[string][]models.Case{"T": {
			{Name: "test-1", Method: "POST", URL: "/orders", Sum: "a", Mocks: []string{"m0"}},
			{Name: "test-2", Method: "POST", URL: "/orders", Sum: "b", Mocks: []string{"m1"}},
			{Name: "test-3", Method: "POST", URL: "/orders", Sum: "b", Mocks: []string{"m2"}},
			{Name: "test-4", Method: "GET", URL: "/a", Mocks: []string{"g1"}},
			{Name: "test-5", Method: "GET", URL: "/a", Mocks: []string{"g2"}},
		}},
	}})
	end := r.Exchange(0, "POST", "/orders")
	sees(t, r, 101, "a POST whose body could be any of the three", "m0", "m1", "m2")
	end()
	end = r.Exchange(0, "POST", "/orders")
	sees(t, r, 101, "the second POST, its body the same as the third's", "m0", "m1")
	end()
	r.Exchange(0, "POST", "/orders")()
	end = r.Exchange(0, "GET", "/a")
	sees(t, r, 101, "the first of two identical GETs", "g1")
	end()
}

func TestAPortlessRequestWhileAnotherPackageRunsIsNotPairedWithItsTest(t *testing.T) {
	f, r := oneTest(caseTable())
	f.spawn(200, 1, at(0), "/tmp/other.test")
	r.Begin(200, "/repo/e2e", "/repo/e2e", true, at(1))
	f.spawn(201, 200, at(2), "/tmp/shop")
	r.Exchange(0, "GET", "/a")()
	end := r.Exchange(0, "GET", "/a")
	defer end()
	sees(t, r, 101, "the open test, a request it may not own in flight", "fx", "m1", "m2", "m3")
}

func TestATestOpenDuringAPlacelessRequestIsServedWholeAfterIt(t *testing.T) {
	f, r := oneTest(caseTable())
	r.Exchange(0, "GET", "/a")()
	f.spawn(200, 1, at(0), "/tmp/other.test")
	r.Begin(200, "/repo/e2e", "/repo/e2e", true, at(1))
	r.Exchange(0, "GET", "/a")()
	delete(f.born, 200)
	end := r.Exchange(0, "GET", "/a")
	defer end()
	sees(t, r, 101, "the next GET /a, once the other package is gone", "fx", "m1", "m2", "m3")
}

func TestAnExtraPollDoesNotTakeALaterCaseByItsPath(t *testing.T) {
	_, r := oneTest(map[string]models.SetTable{"e2e": {
		Tests: map[string][]models.Owned{"T": {{Name: "j1"}, {Name: "j1b"}, {Name: "j2"}, {Name: "j3"}}},
		Cases: map[string][]models.Case{"T": {
			{Name: "test-1", Method: "GET", URL: "/jobs/1", Mocks: []string{"j1"}},
			{Name: "test-2", Method: "GET", URL: "/jobs/1", Mocks: []string{"j1b"}},
			{Name: "test-3", Method: "GET", URL: "/jobs/2", Mocks: []string{"j2"}},
			{Name: "test-4", Method: "GET", URL: "/jobs/3", Mocks: []string{"j3"}},
		}},
	}})
	r.Exchange(0, "GET", "/jobs/1")()
	r.Exchange(0, "GET", "/jobs/1")()
	end := r.Exchange(0, "GET", "/jobs/1")
	sees(t, r, 101, "a third poll of /jobs/1", "j1", "j1b", "j2", "j3")
	end()
	end = r.Exchange(0, "GET", "/jobs/2")
	defer end()
	sees(t, r, 101, "GET /jobs/2", "j2")
}
