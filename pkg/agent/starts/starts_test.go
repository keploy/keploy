package starts

import (
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
